package fetch

import (
	"github.com/go-errors/errors"
	"github.com/itchio/butler/buse"
	"github.com/itchio/butler/database/hades"
	"github.com/itchio/butler/database/models"
)

func FetchCave(rc *buse.RequestContext, params *buse.FetchCaveParams) (*buse.FetchCaveResult, error) {
	consumer := rc.Consumer

	db, err := rc.DB()
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}

	cave, err := models.CaveByID(db, params.CaveID)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}

	if cave != nil {
		err = hades.NewContext(db, consumer).Preload(db, &hades.PreloadParams{
			Record: cave,
			Fields: []hades.PreloadField{
				hades.PreloadField{Name: "Game"},
				hades.PreloadField{Name: "Upload"},
				hades.PreloadField{Name: "Build"},
			},
		})
		if err != nil {
			return nil, errors.Wrap(err, 0)
		}
	}

	res := &buse.FetchCaveResult{
		Cave: formatCave(cave),
	}
	return res, nil
}

func formatCave(cave *models.Cave) *buse.Cave {
	if cave == nil {
		return nil
	}

	return &buse.Cave{
		ID: cave.ID,

		Game:   cave.Game,
		Upload: cave.Upload,
		Build:  cave.Build,

		InstallInfo: &buse.CaveInstallInfo{
			AbsoluteInstallFolder: "<stub>",
			InstalledSize:         cave.InstalledSize,
			InstallLocation:       cave.InstallLocation,
		},

		Stats: &buse.CaveStats{
			InstalledAt:   cave.InstalledAt,
			LastTouchedAt: cave.LastTouchedAt,
			SecondsRun:    cave.SecondsRun,
		},
	}
}