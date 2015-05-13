package daemon

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/pkg/chrootarchive"
	"github.com/docker/docker/runconfig"
	"github.com/docker/docker/volume"
	volumedrivers "github.com/docker/docker/volume/drivers"
)

var localMountErr = fmt.Errorf("Invalid driver: %s driver doesn't support named volumes", volume.DefaultDriverName)

type mountPoint struct {
	Name        string
	Destination string
	Driver      string
	RW          bool
	Volume      volume.Volume `json:"-"`
	source      string
}

func (m *mountPoint) Setup() (string, error) {
	if m.Volume != nil {
		return m.Volume.Mount()
	}

	if len(m.source) > 0 {
		if _, err := os.Stat(m.source); err != nil {
			if !os.IsNotExist(err) {
				return "", err
			}
			if err := os.MkdirAll(m.source, 0755); err != nil {
				return "", err
			}
		}
		return m.source, nil
	}

	return "", fmt.Errorf("Unable to setup mount point, neither source nor volume defined")
}

func (m *mountPoint) Source() string {
	if m.Volume != nil {
		return m.Volume.Path()
	}

	return m.source
}

func parseBindMount(spec string, config *runconfig.Config) (*mountPoint, error) {
	bind := &mountPoint{
		RW: true,
	}
	arr := strings.Split(spec, ":")

	switch len(arr) {
	case 2:
		bind.Destination = arr[1]
	case 3:
		bind.Destination = arr[1]
		if !validMountMode(arr[2]) {
			return nil, fmt.Errorf("invalid mode for volumes-from: %s", arr[2])
		}
		bind.RW = arr[2] == "rw"
	default:
		return nil, fmt.Errorf("Invalid volume specification: %s", spec)
	}

	if !filepath.IsAbs(arr[0]) {
		bind.Driver, bind.Name = parseNamedVolumeInfo(arr[0], config)
		if bind.Driver == volume.DefaultDriverName {
			return nil, localMountErr
		}
	} else {
		bind.source = filepath.Clean(arr[0])
	}

	bind.Destination = filepath.Clean(bind.Destination)
	return bind, nil
}

func parseNamedVolumeInfo(info string, config *runconfig.Config) (driver string, name string) {
	p := strings.SplitN(info, "/", 2)
	switch len(p) {
	case 2:
		driver = p[0]
		name = p[1]
	default:
		if driver = config.VolumeDriver; len(driver) == 0 {
			driver = volume.DefaultDriverName
		}
		name = p[0]
	}

	return
}

func parseVolumesFrom(spec string) (string, string, error) {
	if len(spec) == 0 {
		return "", "", fmt.Errorf("malformed volumes-from specification: %s", spec)
	}

	specParts := strings.SplitN(spec, ":", 2)
	id := specParts[0]
	mode := "rw"

	if len(specParts) == 2 {
		mode = specParts[1]
		if !validMountMode(mode) {
			return "", "", fmt.Errorf("invalid mode for volumes-from: %s", mode)
		}
	}
	return id, mode, nil
}

func validMountMode(mode string) bool {
	validModes := map[string]bool{
		"rw": true,
		"ro": true,
	}
	return validModes[mode]
}

func copyExistingContents(source, destination string) error {
	volList, err := ioutil.ReadDir(source)
	if err != nil {
		return err
	}
	if len(volList) > 0 {
		srcList, err := ioutil.ReadDir(destination)
		if err != nil {
			return err
		}
		if len(srcList) == 0 {
			// If the source volume is empty copy files from the root into the volume
			if err := chrootarchive.CopyWithTar(source, destination); err != nil {
				return err
			}
		}
	}
	return copyOwnership(source, destination)
}

// registerMountPoints initializes the container mount points with the configured volumes and bind mounts.
// It follows the next sequence to decide what to mount in each final destination:
//
// 1. Select the previously configured mount points for the containers, if any.
// 2. Select the volumes mounted from another containers. Overrides previously configured mount point destination.
// 3. Select the bind mounts set by the client. Overrides previously configured mount point destinations.
func (daemon *Daemon) registerMountPoints(container *Container, hostConfig *runconfig.HostConfig) error {
	binds := map[string]bool{}
	mountPoints := map[string]*mountPoint{}

	// 1. Read already configured mount points.
	for name, point := range container.MountPoints {
		mountPoints[name] = point
	}

	// 2. Read volumes from other containers.
	for _, v := range hostConfig.VolumesFrom {
		containerID, mode, err := parseVolumesFrom(v)
		if err != nil {
			return err
		}

		c, err := daemon.Get(containerID)
		if err != nil {
			return err
		}

		for _, m := range c.MountPoints {
			v, err := createVolume(m.Name, m.Driver)
			if err != nil {
				return err
			}

			cp := m
			cp.RW = mode != "ro"
			cp.Volume = v

			mountPoints[cp.Destination] = cp
		}
	}

	// 3. Read bind mounts
	for _, b := range hostConfig.Binds {
		// #10618
		bind, err := parseBindMount(b, container.Config)
		if err != nil {
			return err
		}

		if binds[bind.Destination] {
			return fmt.Errorf("Duplicate bind mount %s", bind.Destination)
		}

		if len(bind.Name) > 0 && len(bind.Driver) > 0 {
			v, err := createVolume(bind.Name, bind.Driver)
			if err != nil {
				return err
			}
			bind.Volume = v
		}

		binds[bind.Destination] = true
		mountPoints[bind.Destination] = bind
	}

	container.MountPoints = mountPoints

	return nil
}

// verifyOldVolumesInfo ports volumes configured for the containers pre docker 1.7.
// It reads the container configuration and creates valid mount points for the old volumes.
func (daemon *Daemon) verifyOldVolumesInfo(container *Container) error {
	jsonPath, err := container.jsonPath()
	if err != nil {
		return err
	}
	f, err := os.Open(jsonPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	type oldContVolCfg struct {
		Volumes   map[string]string
		VolumesRW map[string]bool
	}

	var vols oldContVolCfg
	if err := json.NewDecoder(f).Decode(&vols); err != nil {
		return err
	}

	for destination, hostPath := range vols.Volumes {
		vfsPath := filepath.Join(daemon.root, "vfs", "dir")

		if strings.HasPrefix(hostPath, vfsPath) {
			id := filepath.Base(hostPath)

			container.AddLocalMountPoint(id, destination, vols.VolumesRW[destination])
		}
	}

	return container.ToDisk()
}

func createVolume(name, driverName string) (volume.Volume, error) {
	vd, err := getVolumeDriver(driverName)
	if err != nil {
		return nil, err
	}
	return vd.Create(name)
}

func removeVolume(v volume.Volume) error {
	vd, err := getVolumeDriver(v.DriverName())
	if err != nil {
		return nil
	}
	return vd.Remove(v)
}

func getVolumeDriver(name string) (volume.Driver, error) {
	if name == "" {
		name = volume.DefaultDriverName
	}
	vd := volumedrivers.Lookup(name)
	if vd == nil {
		return nil, fmt.Errorf("Volumes Driver %s isn't registered", name)
	}
	return vd, nil
}
