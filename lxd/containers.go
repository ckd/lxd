package main

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/lxc/go-lxc.v2"

	"github.com/lxc/lxd/shared"
)

type execWs struct {
	command          []string
	container        *lxc.Container
	rootUid          int
	rootGid          int
	options          lxc.AttachOptions
	conns            map[int]*websocket.Conn
	allConnected     chan bool
	controlConnected chan bool
	interactive      bool
	done             chan shared.OperationResult
	fds              map[int]string
}

type commandPostContent struct {
	Command     []string          `json:"command"`
	WaitForWS   bool              `json:"wait-for-websocket"`
	Interactive bool              `json:"interactive"`
	Environment map[string]string `json:"environment"`
}

type containerConfigReq struct {
	Profiles []string          `json:"profiles"`
	Config   map[string]string `json:"config"`
	Devices  shared.Devices    `json:"devices"`
	Restore  string            `json:"restore"`
}

type containerStatePutReq struct {
	Action  string `json:"action"`
	Timeout int    `json:"timeout"`
	Force   bool   `json:"force"`
}

type containerPostBody struct {
	Migration bool   `json:"migration"`
	Name      string `json:"name"`
}

type containerPostReq struct {
	Name      string               `json:"name"`
	Source    containerImageSource `json:"source"`
	Config    map[string]string    `json:"config"`
	Profiles  []string             `json:"profiles"`
	Ephemeral bool                 `json:"ephemeral"`
}

type containerImageSource struct {
	Type string `json:"type"`

	/* for "image" type */
	Alias       string `json:"alias"`
	Fingerprint string `json:"fingerprint"`
	Server      string `json:"server"`
	Secret      string `json:"secret"`

	/*
	 * for "migration" and "copy" types, as an optimization users can
	 * provide an image hash to extract before the filesystem is rsync'd,
	 * potentially cutting down filesystem transfer time. LXD will not go
	 * and fetch this image, it will simply use it if it exists in the
	 * image store.
	 */
	BaseImage string `json:"base-image"`

	/* for "migration" type */
	Mode       string            `json:"mode"`
	Operation  string            `json:"operation"`
	Websockets map[string]string `json:"secrets"`

	/* for "copy" type */
	Source string `json:"source"`
}

var containersCmd = Command{
	name: "containers",
	get:  containersGet,
	post: containersPost,
}

var containerCmd = Command{
	name:   "containers/{name}",
	get:    containerGet,
	put:    containerPut,
	delete: containerDelete,
	post:   containerPost,
}

var containerStateCmd = Command{
	name: "containers/{name}/state",
	get:  containerStateGet,
	put:  containerStatePut,
}

var containerFileCmd = Command{
	name: "containers/{name}/files",
	get:  containerFileHandler,
	post: containerFileHandler,
}

var containerSnapshotsCmd = Command{
	name: "containers/{name}/snapshots",
	get:  containerSnapshotsGet,
	post: containerSnapshotsPost,
}

var containerSnapshotCmd = Command{
	name:   "containers/{name}/snapshots/{snapshotName}",
	get:    snapshotHandler,
	post:   snapshotHandler,
	delete: snapshotHandler,
}

var containerExecCmd = Command{
	name: "containers/{name}/exec",
	post: containerExecPost,
}

func containerWatchEphemeral(d *Daemon, c container) {
	go func() {
		lxContainer, err := c.LXContainerGet()
		if err != nil {
			return
		}

		lxContainer.Wait(lxc.STOPPED, -1*time.Second)
		lxContainer.Wait(lxc.RUNNING, 1*time.Second)
		lxContainer.Wait(lxc.STOPPED, -1*time.Second)

		_, err = dbContainerIDGet(d.db, c.NameGet())
		if err != nil {
			return
		}

		c.Delete()
	}()
}

func containersWatch(d *Daemon) error {
	q := fmt.Sprintf("SELECT name FROM containers WHERE type=?")
	inargs := []interface{}{cTypeRegular}
	var name string
	outfmt := []interface{}{name}

	result, err := dbQueryScan(d.db, q, inargs, outfmt)
	if err != nil {
		return err
	}

	for _, r := range result {
		container, err := newLxdContainer(string(r[0].(string)), d)
		if err != nil {
			return err
		}

		if container.IsEmpheral() && container.IsRunning() {
			containerWatchEphemeral(d, container)
		}
	}

	/*
	 * force collect the containers we created above; see comment in
	 * daemon.go:createCmd.
	 */
	runtime.GC()

	return nil
}

func containersRestart(d *Daemon) error {
	q := fmt.Sprintf("SELECT name FROM containers WHERE type=? AND power_state=1")
	inargs := []interface{}{cTypeRegular}
	var name string
	outfmt := []interface{}{name}

	result, err := dbQueryScan(d.db, q, inargs, outfmt)
	if err != nil {
		return err
	}

	_, err = dbExec(d.db, "UPDATE containers SET power_state=0")
	if err != nil {
		return err
	}

	for _, r := range result {
		container, err := newLxdContainer(string(r[0].(string)), d)
		if err != nil {
			return err
		}

		container.Start()
	}

	return nil
}

func containersShutdown(d *Daemon) error {
	results, err := d.ListRegularContainers()
	if err != nil {
		return err
	}

	var wg sync.WaitGroup

	for _, r := range results {
		container, err := newLxdContainer(r, d)
		if err != nil {
			return err
		}

		if container.IsRunning() {
			_, err = dbExec(
				d.db,
				"UPDATE containers SET power_state=1 WHERE name=?",
				container.NameGet())
			if err != nil {
				return err
			}

			wg.Add(1)
			go func() {
				container.Shutdown(time.Second * 30)
				container.Stop()
				wg.Done()
			}()
		}
		wg.Wait()
	}

	return nil
}

func containerDeleteSnapshots(d *Daemon, cname string) error {
	prefix := cname + shared.SnapshotDelimiter
	length := len(prefix)
	q := "SELECT name, id FROM containers WHERE type=? AND SUBSTR(name,1,?)=?"
	var id int
	var sname string
	inargs := []interface{}{cTypeSnapshot, length, prefix}
	outfmt := []interface{}{sname, id}
	results, err := dbQueryScan(d.db, q, inargs, outfmt)
	if err != nil {
		return err
	}

	var ids []int

	backingFs, err := filesystemDetect(shared.VarPath("containers", cname))
	if err != nil && !os.IsNotExist(err) {
		shared.Debugf("Error cleaning up snapshots: %s\n", err)
		return err
	}

	for _, r := range results {
		sname = r[0].(string)
		id = r[1].(int)
		ids = append(ids, id)
		cdir := shared.VarPath("snapshots", sname)

		if backingFs == "btrfs" {
			btrfsDeleteSubvol(cdir)
		}
		os.RemoveAll(cdir)
	}

	for _, id := range ids {
		_, err = dbExec(d.db, "DELETE FROM containers WHERE id=?", id)
		if err != nil {
			return err
		}
	}

	return nil
}

/*
 * This is called by lxd when called as "lxd forkstart <container>"
 * 'forkstart' is used instead of just 'start' in the hopes that people
 * do not accidentally type 'lxd start' instead of 'lxc start'
 *
 * We expect to read the lxcconfig over fd 3.
 */
func startContainer(args []string) error {
	if len(args) != 4 {
		return fmt.Errorf("Bad arguments: %q\n", args)
	}
	name := args[1]
	lxcpath := args[2]
	configPath := args[3]

	c, err := lxc.NewContainer(name, lxcpath)
	if err != nil {
		return fmt.Errorf("Error initializing container for start: %q", err)
	}
	err = c.LoadConfigFile(configPath)
	if err != nil {
		return fmt.Errorf("Error opening startup config file: %q", err)
	}

	err = c.Start()
	if err != nil {
		os.Remove(configPath)
	} else {
		shared.FileMove(configPath, shared.LogPath(name, "lxc.conf"))
	}

	return err
}
