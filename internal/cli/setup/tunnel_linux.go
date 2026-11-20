package setup

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

func isolateTunnelProcess(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if os.Geteuid() != 0 {
		return nil
	}
	account, err := user.Lookup("nobody")
	if err != nil {
		return errors.New("cloudflared requires a local unprivileged nobody account")
	}
	uid, uidErr := strconv.ParseUint(account.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(account.Gid, 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		return errors.New("invalid nobody account for cloudflared")
	}
	command.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	command.Dir = "/"
	command.Env = []string{"HOME=/nonexistent"}
	return nil
}
