//go:build !linux || (!amd64 && !arm64)

package vm

import "errors"

func NewQEMU(QEMUConfig) (Backend, error) {
	return nil, errors.New("QEMU execution requires a Linux amd64 or arm64 host with KVM")
}
