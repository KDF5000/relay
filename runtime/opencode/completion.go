package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/KDF5000/relay/runtime/agentcli"
	runtimeprocess "github.com/KDF5000/relay/runtime/process"
)

func successfulFinish(reason string) bool {
	return reason == "stop" || reason == "end_turn"
}

type exportedMessage struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Role   string `json:"role"`
	Finish string `json:"finish"`
	Time   struct {
		Completed int64 `json:"completed"`
	} `json:"time"`
	Error json.RawMessage  `json:"error"`
	Info  *exportedMessage `json:"info"`
}

func verifyNativeCompletion(ctx context.Context, binary string, config Config, run agentcli.Run, sessionID, messageID string) (string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(checkCtx, binary, "session", "export", "--standalone", sessionID)
	runtimeprocess.Configure(command)
	command.Dir = run.WorkDir
	command.Env = environment(config, run)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr := &agentcli.LimitedBuffer{Remaining: 64 << 10}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return "", fmt.Errorf("verify completion: %w", err)
	}
	reaped := false
	defer func() {
		cancel()
		if !reaped {
			_ = command.Wait()
		}
	}()
	reason, readErr := readNativeCompletion(json.NewDecoder(stdout), sessionID, messageID)
	if readErr != nil {
		cancel()
	}
	waitErr := command.Wait()
	reaped = true
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("verify completion: %w", checkCtx.Err())
	}
	if readErr != nil {
		return "", fmt.Errorf("verify completion: %w", readErr)
	}
	if checkCtx.Err() != nil {
		return "", fmt.Errorf("verify completion: %w", checkCtx.Err())
	}
	if waitErr != nil {
		return "", fmt.Errorf("verify completion: export failed: %w: %s", waitErr, agentcli.Truncate(stderr.String(), 4096))
	}
	return reason, nil
}

// Decode one message at a time, discarding transcript content. Long session
// histories must not be retained in memory or copied into diagnostics.
func readNativeCompletion(decoder *json.Decoder, sessionID, messageID string) (string, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", err
	}
	if token != json.Delim('{') {
		return "", errors.New("invalid session export")
	}
	var exportedSessionID string
	var matched *exportedMessage
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return "", err
		}
		switch key {
		case "info":
			var info struct {
				ID string `json:"id"`
			}
			if err := decoder.Decode(&info); err != nil {
				return "", err
			}
			exportedSessionID = info.ID
		case "messages":
			start, err := decoder.Token()
			if err != nil || start != json.Delim('[') {
				return "", errors.New("invalid exported messages")
			}
			for decoder.More() {
				var message exportedMessage
				if err := decoder.Decode(&message); err != nil {
					return "", err
				}
				if message.Info != nil {
					message = *message.Info
				}
				if message.ID == messageID {
					if matched != nil {
						return "", errors.New("duplicate exported message ID")
					}
					matched = &message
				}
			}
			if _, err := decoder.Token(); err != nil {
				return "", err
			}
		default:
			var discard json.RawMessage
			if err := decoder.Decode(&discard); err != nil {
				return "", err
			}
		}
	}
	if _, err := decoder.Token(); err != nil {
		return "", err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("unexpected trailing session export data")
	}
	if exportedSessionID != sessionID {
		return "", errors.New("session export identity mismatch")
	}
	if matched == nil {
		return "", fmt.Errorf("message %s is absent from session export", messageID)
	}
	role := matched.Type
	if role == "" {
		role = matched.Role
	}
	if role != "assistant" || matched.Time.Completed <= 0 || !successfulFinish(matched.Finish) || len(matched.Error) > 0 && strings.TrimSpace(string(matched.Error)) != "null" {
		return "", fmt.Errorf("message %s is not a successfully completed assistant reply (reason %q)", messageID, matched.Finish)
	}
	return matched.Finish, nil
}
