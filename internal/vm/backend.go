package vm

import (
	"context"
	"errors"
)

// ErrImageInfrastructure identifies failures attributable to a VM base image.
var ErrImageInfrastructure = errors.New("VM base image infrastructure failure")

// Instance identifies resources across daemon restarts.
type Instance struct {
	ID, Backend, Image, Resource               string
	ImageRevision, RunnerVersion, RunnerSHA256 string `json:",omitempty"`
	Namespace                                  string `json:",omitempty"`
	Scope, ScaleSet                            string
	ScaleSetID, RunnerID                       int
	LocalReclaimed                             bool `json:",omitempty"`
}

type Process struct {
	Ready  <-chan struct{}
	Exited <-chan error
}

type Backend interface {
	Check(context.Context) error
	Prepare(context.Context, Instance) error
	Start(context.Context, Instance, string) (Process, error)
	Stop(context.Context, Instance) error
	Delete(context.Context, Instance) error
}
