//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows))

package container

import (
	"context"

	"github.com/docker/docker/api/types"
	"github.com/nektos/act/pkg/common"
)

func NewDockerNetworkCreateExecutor(name string, inheritDriverOpts []string) common.Executor {
	return func(ctx context.Context) error {
		cli, err := GetDockerClient(ctx)
		if err != nil {
			return err
		}

		createOpts := types.NetworkCreate{
			Driver: "bridge",
			Scope:  "local",
		}

		if len(inheritDriverOpts) > 0 {
			network, err := cli.NetworkInspect(ctx, "bridge", types.NetworkInspectOptions{Scope: "local"})
			if err != nil {
				return err
			}
			createOpts.Options = make(map[string]string, len(inheritDriverOpts))
			for _, optKey := range inheritDriverOpts {
				if val, ok := network.Options[optKey]; ok {
					createOpts.Options[optKey] = val
				}
			}
		}

		_, err = cli.NetworkCreate(ctx, name, createOpts)
		if err != nil {
			return err
		}

		return nil
	}
}

func NewDockerNetworkRemoveExecutor(name string) common.Executor {
	return func(ctx context.Context) error {
		cli, err := GetDockerClient(ctx)
		if err != nil {
			return err
		}

		return cli.NetworkRemove(ctx, name)
	}
}
