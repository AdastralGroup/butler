package msi

import (
	"github.com/go-errors/errors"
	"github.com/itchio/butler/installer"
)

func (m *Manager) Uninstall(params *installer.UninstallParams) error {
	consumer := params.Consumer
	receipt := params.Receipt

	if receipt == nil {
		return errors.New("Missing receipt, don't know what to uninstall")
	}

	if receipt.MSIProductCode == "" {
		return errors.New("Missing product code in receipt, don't know what to uninstall")
	}

	args := []string{
		"msi-uninstall",
		receipt.MSIProductCode,
	}

	consumer.Infof("Attempting non-elevated MSI uninstall")
	res, err := installer.RunSelf(consumer, args)
	if err != nil {
		return errors.Wrap(err, 0)
	}

	if res.ExitCode != 0 {
		if shouldTryElevated(consumer, res) {
			args = append(args, "--elevate")

			consumer.Infof("Attempting elevated MSI uninstall")
			res, err := installer.RunSelf(consumer, args)
			if err != nil {
				return errors.Wrap(err, 0)
			}

			if res.ExitCode != 0 {
				consumer.Errorf("Elevated MSI install failed, we're out of options")
				return errors.New("Elevated MSI installation failed, this package is probably not compatible")
			}
		}
	}

	return nil
}
