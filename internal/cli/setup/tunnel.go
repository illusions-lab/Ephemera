package setup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var quickTunnelURL = regexp.MustCompile(`\bhttps://[a-z0-9-]+\.trycloudflare\.com\b`)

const installedCloudflared = "/opt/ephemera/tools/cloudflared"

type quickTunnel struct {
	address *url.URL
	done    <-chan struct{}
	stop    context.CancelFunc
}

func startQuickTunnel(ctx context.Context, address string) (*quickTunnel, error) {
	binary := installedCloudflared
	if _, err := os.Stat(binary); errors.Is(err, os.ErrNotExist) {
		var err error
		binary, err = exec.LookPath("cloudflared")
		if err != nil {
			return nil, errors.New("install cloudflared to use a Cloudflare Quick Tunnel")
		}
	}

	processCtx, cancel := context.WithCancel(ctx)
	command := exec.CommandContext(processCtx, binary, "tunnel", "--config", os.DevNull, "--no-autoupdate", "--url", "http://"+address)
	if err := isolateTunnelProcess(command); err != nil {
		cancel()
		return nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		_ = output.Close()
		cancel()
		return nil, err
	}

	assigned := make(chan *url.URL, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		stopClosing := context.AfterFunc(processCtx, func() { _ = output.Close() })
		defer stopClosing()
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			match := quickTunnelURL.FindString(scanner.Text())
			if match == "" || match == "https://api.trycloudflare.com" {
				continue
			}
			if address, err := url.Parse(match); err == nil {
				select {
				case assigned <- address:
				default:
				}
			}
		}
		_ = output.Close()
		_ = command.Wait()
	}()

	tunnel := &quickTunnel{done: done, stop: cancel}
	timer := time.NewTimer(25 * time.Second)
	defer timer.Stop()
	select {
	case tunnel.address = <-assigned:
		return tunnel, nil
	case <-done:
		cancel()
		return nil, errors.New("cloudflared exited before assigning a Quick Tunnel URL")
	case <-timer.C:
		tunnel.close()
		return nil, errors.New("timed out starting the Cloudflare Quick Tunnel")
	case <-ctx.Done():
		tunnel.close()
		return nil, ctx.Err()
	}
}

func (t *quickTunnel) close() {
	t.stop()
	<-t.done
}

func showBrowserSetup(ctx context.Context, address, localURL, publicURL string) {
	if publicURL != "" {
		fmt.Printf("\nCloudflare Quick Tunnel\n  %s\n\n", publicURL)
	} else {
		fmt.Printf("\nOpen this address in your browser\n  %s\n\n", localURL)
	}

	ip := publicIP(ctx)
	host := ip
	if ip == "" {
		ip, host = "unavailable", "VM_IP"
	}
	fmt.Printf("Host public IP (outbound): %s\n", ip)
	if publicURL != "" {
		fmt.Println("SSH forwarding alternative")
	}
	fmt.Printf("If SSH is reachable, run this on your computer (replace user and address as needed)\n  ssh -N -L %s:%s user@%s\n", address, address, host)
	fmt.Printf("Then open\n  %s\n\n", localURL)
	fmt.Println("Waiting for GitHub App installation. Press Ctrl+C to cancel.")
}

func publicIP(ctx context.Context) string {
	client := &http.Client{Timeout: 3 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.cloudflare.com/cdn-cgi/trace", nil)
	if err != nil {
		return ""
	}
	response, err := client.Do(request)
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ""
	}
	scanner := bufio.NewScanner(io.LimitReader(response.Body, 4096))
	for scanner.Scan() {
		if value, ok := strings.CutPrefix(scanner.Text(), "ip="); ok {
			if ip := net.ParseIP(value); ip != nil {
				return ip.String()
			}
		}
	}
	return ""
}
