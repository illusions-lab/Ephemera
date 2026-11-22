package vm

import "bytes"

// Bound subprocess diagnostics separately from guest serial output.
type boundedVMOutput struct{ output bytes.Buffer }

func (b *boundedVMOutput) Write(p []byte) (int, error) {
	const limit = 4096
	_, _ = b.output.Write(p[:min(len(p), limit-b.output.Len())])
	return len(p), nil
}
