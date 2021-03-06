package main

import (
	"time"

	"github.com/lxc/lxd/lxd/db"
	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/migration"
	"github.com/lxc/lxd/shared"
)

func snapshotProtobufToInstanceArgs(project string, containerName string, snap *migration.Snapshot) db.InstanceArgs {
	config := map[string]string{}

	for _, ent := range snap.LocalConfig {
		config[ent.GetKey()] = ent.GetValue()
	}

	devices := deviceConfig.Devices{}
	for _, ent := range snap.LocalDevices {
		props := map[string]string{}
		for _, prop := range ent.Config {
			props[prop.GetKey()] = prop.GetValue()
		}

		devices[ent.GetName()] = props
	}

	name := containerName + shared.SnapshotDelimiter + snap.GetName()
	args := db.InstanceArgs{
		Architecture: int(snap.GetArchitecture()),
		Config:       config,
		Type:         instancetype.Container,
		Snapshot:     true,
		Devices:      devices,
		Ephemeral:    snap.GetEphemeral(),
		Name:         name,
		Profiles:     snap.Profiles,
		Stateful:     snap.GetStateful(),
		Project:      project,
	}

	if snap.GetCreationDate() != 0 {
		args.CreationDate = time.Unix(snap.GetCreationDate(), 0)
	}

	if snap.GetLastUsedDate() != 0 {
		args.LastUsedDate = time.Unix(snap.GetLastUsedDate(), 0)
	}

	return args
}
