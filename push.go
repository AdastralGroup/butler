package main

import (
	"archive/zip"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/go-errors/errors"
	"github.com/itchio/butler/comm"
	"github.com/itchio/go-itchio"
	"github.com/itchio/wharf/counter"
	"github.com/itchio/wharf/pwr"
	"github.com/itchio/wharf/sync"
	"github.com/itchio/wharf/tlc"
	"github.com/itchio/wharf/uploader"
)

func push(buildPath string, spec string, userVersion string, fixPerms bool) {
	go versionCheck()
	must(doPush(buildPath, spec, userVersion, fixPerms))
}

func doPush(buildPath string, spec string, userVersion string, fixPerms bool) error {
	spec = strings.ToLower(spec)

	// start walking source container while waiting on auth flow
	sourceContainerChan := make(chan walkResult)
	walkErrs := make(chan error)
	go doWalk(buildPath, sourceContainerChan, walkErrs, fixPerms)

	target, channel, err := parseSpec(spec)
	if err != nil {
		return errors.Wrap(err, 1)
	}

	client, err := authenticateViaOauth()
	if err != nil {
		return errors.Wrap(err, 1)
	}

	newBuildRes, err := client.CreateBuild(target, channel, userVersion)
	if err != nil {
		return errors.Wrap(err, 1)
	}

	buildID := newBuildRes.Build.ID
	parentID := newBuildRes.Build.ParentBuild.ID

	var targetSignature []sync.BlockHash
	var targetContainer *tlc.Container

	if parentID == 0 {
		comm.Opf("For channel `%s`: pushing first build", channel)
		targetSignature = make([]sync.BlockHash, 0)
		targetContainer = &tlc.Container{}
	} else {
		comm.Opf("For channel `%s`: last build is %d, downloading its signature", channel, parentID)
		var buildFiles itchio.ListBuildFilesResponse
		buildFiles, err = client.ListBuildFiles(parentID)
		if err != nil {
			return errors.Wrap(err, 1)
		}

		var signatureFileID int64
		for _, f := range buildFiles.Files {
			if f.Type == itchio.BuildFileType_SIGNATURE {
				signatureFileID = f.ID
				break
			}
		}

		if signatureFileID == 0 {
			comm.Dief("Could not find signature for parent build %d, aborting", parentID)
		}

		var signatureReader io.Reader
		signatureReader, err = client.DownloadBuildFile(parentID, signatureFileID)
		if err != nil {
			return errors.Wrap(err, 1)
		}

		targetContainer, targetSignature, err = pwr.ReadSignature(signatureReader)
		if err != nil {
			return errors.Wrap(err, 1)
		}
	}

	newPatchRes, newSignatureRes, err := createBothFiles(client, buildID)
	if err != nil {
		return errors.Wrap(err, 1)
	}

	uploadDone := make(chan bool)
	uploadErrs := make(chan error)

	patchWriter, err := uploader.NewResumableUpload(newPatchRes.File.UploadURL,
		uploadDone, uploadErrs, comm.NewStateConsumer())
	patchWriter.MaxChunkGroup = *appArgs.maxChunkGroup
	if err != nil {
		return errors.Wrap(err, 1)
	}

	signatureWriter, err := uploader.NewResumableUpload(newSignatureRes.File.UploadURL,
		uploadDone, uploadErrs, comm.NewStateConsumer())
	signatureWriter.MaxChunkGroup = *appArgs.maxChunkGroup
	if err != nil {
		return errors.Wrap(err, 1)
	}

	comm.Debugf("Launching patch & signature channels")

	patchCounter := counter.NewWriter(patchWriter)
	signatureCounter := counter.NewWriter(signatureWriter)

	// we started walking the source container in the beginning,
	// we actually need it now.
	// note that we could actually start diffing before all the file
	// creation & upload setup is done

	var sourceContainer *tlc.Container
	var sourcePool sync.FilePool

	comm.Debugf("Waiting for source container")
	select {
	case walkErr := <-walkErrs:
		return errors.Wrap(walkErr, 1)
	case walkies := <-sourceContainerChan:
		comm.Debugf("Got sourceContainer!")
		sourceContainer = walkies.container
		sourcePool = walkies.pool
		break
	}

	comm.Logf("")
	comm.Opf("Pushing %s (%s)", humanize.IBytes(uint64(sourceContainer.Size)), sourceContainer.Stats())

	comm.Debugf("Building diff context")
	var readBytes int64

	bytesPerSec := float64(0)
	lastUploadedBytes := int64(0)
	stopTicking := make(chan struct{})

	updateProgress := func() {
		uploadedBytes := int64(float64(patchWriter.UploadedBytes))

		// input bytes that aren't in output, for esxample:
		//  - bytes that have been compressed away
		//  - bytes that were in old build and were simply reused
		goneBytes := readBytes - patchWriter.TotalBytes

		conservativeTotalBytes := sourceContainer.Size - goneBytes

		leftBytes := conservativeTotalBytes - uploadedBytes
		if leftBytes > 10*1024 {
			netStatus := "- network idle"
			if bytesPerSec > 1 {
				netStatus = fmt.Sprintf("@ %s/s", humanize.IBytes(uint64(bytesPerSec)))
			}
			comm.ProgressLabel(fmt.Sprintf("%s, %s left", netStatus, humanize.IBytes(uint64(leftBytes))))
		} else {
			comm.ProgressLabel(fmt.Sprintf("- almost there"))
		}

		conservativeProgress := float64(uploadedBytes) / float64(conservativeTotalBytes)
		conservativeProgress = min(1.0, conservativeProgress)
		comm.Progress(conservativeProgress)

		comm.ProgressScale(float64(readBytes) / float64(sourceContainer.Size))
	}

	go func() {
		ticker := time.NewTicker(time.Second * time.Duration(2))
		for {
			select {
			case <-ticker.C:
				bytesPerSec = float64(patchWriter.UploadedBytes-lastUploadedBytes) / 2.0
				lastUploadedBytes = patchWriter.UploadedBytes
				updateProgress()
			case <-stopTicking:
				break
			}
		}
	}()

	patchWriter.OnProgress = updateProgress

	stateConsumer := &pwr.StateConsumer{
		OnProgress: func(progress float64) {
			readBytes = int64(float64(sourceContainer.Size) * progress)
			updateProgress()
		},
	}

	dctx := &pwr.DiffContext{
		Compression: &pwr.CompressionSettings{
			Algorithm: pwr.CompressionAlgorithm_BROTLI,
			Quality:   1,
		},

		SourceContainer: sourceContainer,
		FilePool:        sourcePool,

		TargetContainer: targetContainer,
		TargetSignature: targetSignature,

		Consumer: stateConsumer,
	}

	comm.StartProgress()
	comm.ProgressScale(0.0)
	err = dctx.WritePatch(patchCounter, signatureCounter)
	if err != nil {
		return errors.Wrap(err, 1)
	}

	// close in a goroutine to avoid deadlocking
	doClose := func(c io.Closer, done chan bool, errs chan error) {
		closeErr := c.Close()
		if closeErr != nil {
			errs <- errors.Wrap(closeErr, 1)
			return
		}

		done <- true
	}

	go doClose(patchWriter, uploadDone, uploadErrs)
	go doClose(signatureWriter, uploadDone, uploadErrs)

	for c := 0; c < 4; c++ {
		select {
		case uploadErr := <-uploadErrs:
			return errors.Wrap(uploadErr, 1)
		case <-uploadDone:
			comm.Debugf("upload done")
		}
	}

	close(stopTicking)
	comm.ProgressLabel("finalizing build")

	finalDone := make(chan bool)
	finalErrs := make(chan error)

	doFinalize := func(fileID int64, fileSize int64, done chan bool, errs chan error) {
		_, err = client.FinalizeBuildFile(buildID, fileID, fileSize)
		if err != nil {
			errs <- errors.Wrap(err, 1)
			return
		}

		done <- true
	}

	go doFinalize(newPatchRes.File.ID, patchCounter.Count(), finalDone, finalErrs)
	go doFinalize(newSignatureRes.File.ID, signatureCounter.Count(), finalDone, finalErrs)

	for i := 0; i < 2; i++ {
		select {
		case err := <-finalErrs:
			return errors.Wrap(err, 1)
		case <-finalDone:
		}
	}

	comm.EndProgress()

	{
		prettyPatchSize := humanize.IBytes(uint64(patchCounter.Count()))
		percReused := 100.0 * float64(dctx.ReusedBytes) / float64(dctx.FreshBytes+dctx.ReusedBytes)
		relToNew := 100.0 * float64(patchCounter.Count()) / float64(sourceContainer.Size)
		prettyFreshSize := humanize.IBytes(uint64(dctx.FreshBytes))
		savings := 100.0 - relToNew

		if dctx.ReusedBytes > 0 {
			comm.Statf("Re-used %.2f%% of old, added %s fresh data", percReused, prettyFreshSize)
		} else {
			comm.Statf("Added %s fresh data", prettyFreshSize)
		}

		if savings > 0 && !math.IsNaN(savings) {
			comm.Statf("%s patch (%.2f%% savings)", prettyPatchSize, 100.0-relToNew)
		} else {
			comm.Statf("%s patch (no savings)", prettyPatchSize)
		}
	}
	comm.Opf("Build is now processing, should be up in a bit (see `butler status`)")

	return nil
}

type fileSlot struct {
	Type     itchio.BuildFileType
	Response itchio.NewBuildFileResponse
}

func createBothFiles(client *itchio.Client, buildID int64) (patch itchio.NewBuildFileResponse, signature itchio.NewBuildFileResponse, err error) {
	createFile := func(buildType itchio.BuildFileType, done chan fileSlot, errs chan error) {
		var res itchio.NewBuildFileResponse
		res, err = client.CreateBuildFile(buildID, buildType, itchio.BuildFileSubType_DEFAULT, itchio.UploadType_DEFERRED_RESUMABLE)
		if err != nil {
			errs <- errors.Wrap(err, 1)
		}
		comm.Debugf("Created %s build file: %+v", buildType, res.File)

		req, reqErr := http.NewRequest("POST", res.File.UploadURL, nil)
		if reqErr != nil {
			errs <- errors.Wrap(reqErr, 1)
		}

		req.ContentLength = 0

		for k, v := range res.File.UploadHeaders {
			req.Header.Add(k, v)
		}

		gcsRes, gcsErr := client.HTTPClient.Do(req)
		if gcsErr != nil {
			errs <- errors.Wrap(gcsErr, 1)
		}

		if gcsRes.StatusCode != 201 {
			errs <- errors.Wrap(fmt.Errorf("could not create resumable upload session (got HTTP %d)", gcsRes.StatusCode), 1)
		}

		comm.Debugf("Started resumable upload session %s", gcsRes.Header.Get("Location"))

		res.File.UploadHeaders = nil
		res.File.UploadURL = gcsRes.Header.Get("Location")

		done <- fileSlot{buildType, res}
	}

	done := make(chan fileSlot)
	errs := make(chan error)

	go createFile(itchio.BuildFileType_PATCH, done, errs)
	go createFile(itchio.BuildFileType_SIGNATURE, done, errs)

	for i := 0; i < 2; i++ {
		select {
		case err = <-errs:
			err = errors.Wrap(err, 1)
			return
		case slot := <-done:
			switch slot.Type {
			case itchio.BuildFileType_PATCH:
				patch = slot.Response
			case itchio.BuildFileType_SIGNATURE:
				signature = slot.Response
			}
		}
	}

	return
}

type walkResult struct {
	container *tlc.Container
	pool      sync.FilePool
}

func doWalk(path string, out chan walkResult, errs chan error, fixPerms bool) {
	var result walkResult

	stats, err := os.Lstat(path)
	if err != nil {
		errs <- errors.Wrap(err, 1)
		return
	}

	if stats.IsDir() {
		container, err := tlc.Walk(path, filterPaths)
		if err != nil {
			errs <- errors.Wrap(err, 1)
			return
		}

		result = walkResult{
			container: container,
			pool:      container.NewFilePool(path),
		}
	} else {
		sourceReader, err := os.Open(path)
		if err != nil {
			errs <- errors.Wrap(err, 1)
			return
		}

		zr, err := zip.NewReader(sourceReader, stats.Size())
		if err != nil {
			errs <- errors.Wrap(err, 1)
			return
		}

		container, err := tlc.WalkZip(zr, filterPaths)
		if err != nil {
			errs <- errors.Wrap(err, 1)
			return
		}

		result = walkResult{
			container: container,
			pool:      container.NewZipPool(zr),
		}
	}

	if fixPerms {
		result.container.FixPermissions(result.pool)
	}
	out <- result
}

func parseSpec(spec string) (string, string, error) {
	tokens := strings.Split(spec, ":")

	if len(tokens) == 1 {
		return "", "", fmt.Errorf("invalid spec: %s, missing channel (examples: %s:windows-32-beta, %s:linux-64)", spec, spec, spec)
	} else if len(tokens) != 2 {
		return "", "", fmt.Errorf("invalid spec: %s, expected something of the form user/page:channel", spec)
	}

	return tokens[0], tokens[1], nil
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
