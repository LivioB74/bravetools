package platform

import "fmt"

// Backend ..
type Backend interface {
	BraveBackendInit() error
	Info() (Info, error)
	Running() (bool, error)
	Start() error
}

// Info describes Brave Platform
type Info struct {
	ImageStorage  string
	VolumeStorage string
	Name          string
	State         string
	IPv4          string
	Release       string
	ImageHash     string
	Load          string
	Disk          StorageUsage
	Memory        StorageUsage
	CPU           string
}

func NewInfo() Info {
	return Info{
		Disk:   StorageUsage{"Unknown", "Unknown"},
		Memory: StorageUsage{"Unknown", "Unknown"},
		CPU:    "Unknown",
	}
}

type StorageUsage struct {
	UsedStorage  string
	TotalStorage string
}

// NewHostBackend returns a new Backend from provided host Settings
func NewHostBackend(hostSetings HostSettings) (backend Backend, err error) {
	backendType := hostSetings.BackendSettings.Type

	switch backendType {
	case "multipass":
		backend = NewMultipass(hostSetings)
	case "lxd":
		backend = NewLxd(hostSetings)
	default:
		err = fmt.Errorf("backend type %q not supported", backendType)
	}
	return backend, err
}
