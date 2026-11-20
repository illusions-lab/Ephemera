//go:build !linux || !amd64

package vm

import "errors"

func NewQEMU(QEMUConfig) (Backend, error) {
	return nil, errors.New("QEMU execution requires a Linux amd64 host with KVM")
}
