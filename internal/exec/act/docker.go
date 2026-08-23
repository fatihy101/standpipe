package act

import (
	"bytes"
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
)

// docker runs the Docker CLI for image provisioning and assertions.
//
// This environment is NOT act's. It is a separate, equally narrow allowlist
// for a different process, and the token rule — that no GH_TOKEN /
// GITHUB_TOKEN / GH_CONFIG_DIR is inherited — is applied here too even though
// that rule is about act: a credential that cannot reach act should not reach the
// image path either.
//
// DOCKER_CONFIG is passed through when the executor's own process has one. On
// macOS `docker pull` hangs on the keychain credential helper, and pointing
// DOCKER_CONFIG at a directory containing `{}` bypasses it; that is a host
// fact, not a Standpipe configuration key.
func (e *Executor) docker(ctx context.Context, args ...string) (string, error) {
	cmd := osexec.CommandContext(ctx, e.dockerBinary, args...)
	cmd.Env = e.dockerEnv
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.String(), fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.String(), nil
}

func dockerEnvironment(home, dockerHost string) []string {
	env := []string{
		"PATH=" + NarrowPath,
		"HOME=" + home,
	}
	if dockerHost != "" {
		env = append(env, "DOCKER_HOST="+dockerHost)
	}
	if dc := os.Getenv("DOCKER_CONFIG"); dc != "" {
		env = append(env, "DOCKER_CONFIG="+dc)
	}
	return env
}
