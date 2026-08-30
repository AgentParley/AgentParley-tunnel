package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/agentparley/tunnel/internal/sessions"
	"github.com/agentparley/tunnel/internal/wsproto"
)

// runWs is the `agentparley ws [agent] read|write <workspacePath>` subcommand: a thin client that streams a
// workspace file to stdout (read) or from stdin (write) by asking the local daemon — over its unix socket — for a
// presigned S3 URL and moving the bytes host<->S3 directly. It holds no credentials of its own; the daemon carries
// the request up its authenticated tunnel and the platform authorizes it. Errors exit nonzero (2 for a missing
// file, 1 otherwise) with a plain message on stderr, so a shell pipeline sees a normal failed command.
func runWs(args []string) error {
	selector, verb, path, err := parseWsArgs(args)
	if err != nil {
		return err
	}

	socketPath := workspaceSocketPath()
	sessionID, _ := strconv.ParseInt(os.Getenv("AGENTPARLEY_SESSION_ID"), 10, 64)

	switch verb {
	case "read":
		exitOnWsFailure(wsRead(socketPath, sessionID, selector, path))
	case "write":
		exitOnWsFailure(wsWrite(socketPath, sessionID, selector, path))
	}
	return nil
}

func parseWsArgs(args []string) (selector, verb, path string, err error) {
	usage := errors.New("usage: agentparley ws [agent] read|write <workspacePath>")
	if len(args) >= 1 && (args[0] == "read" || args[0] == "write") {
		if len(args) != 2 {
			return "", "", "", usage
		}
		return "", args[0], args[1], nil
	}
	if len(args) == 3 && (args[1] == "read" || args[1] == "write") {
		return args[0], args[1], args[2], nil
	}
	return "", "", "", usage
}

// workspaceSocketPath is the injected socket for a command run inside a turn, falling back to the well-known
// state-dir path so a human shell on the box works too.
func workspaceSocketPath() string {
	if injected := os.Getenv("AGENTPARLEY_WS_SOCKET"); injected != "" {
		return injected
	}
	home, _ := os.UserHomeDir()
	return sessions.WorkspaceSocketPath(home)
}

func wsRead(socketPath string, sessionID int64, selector, path string) error {
	response, err := wsCall(socketPath, wsproto.Request{Op: "read", SessionID: sessionID, AgentSelector: selector, Path: path})
	if err != nil {
		return err
	}
	if !response.OK {
		return wsFailureFrom(response)
	}

	httpResponse, err := http.Get(response.URL)
	if err != nil {
		return &wsFailure{exitCode: 1, message: fmt.Sprintf("downloading from storage: %v", err)}
	}
	defer httpResponse.Body.Close()
	// Gate on status BEFORE streaming: a non-2xx (a file deleted between the row check and the GET, an expired URL)
	// must not dump S3's XML error body into the caller's redirected output file.
	if httpResponse.StatusCode/100 != 2 {
		return &wsFailure{exitCode: 1, message: fmt.Sprintf("storage returned HTTP %d", httpResponse.StatusCode)}
	}
	if _, err := io.Copy(os.Stdout, httpResponse.Body); err != nil {
		return &wsFailure{exitCode: 1, message: fmt.Sprintf("streaming file: %v", err)}
	}
	return nil
}

func wsWrite(socketPath string, sessionID int64, selector, path string) error {
	tempFile, err := os.CreateTemp("", "agentparley-ws-*")
	if err != nil {
		return err
	}
	defer os.Remove(tempFile.Name())

	hasher := sha256.New()
	sizeBytes, err := io.Copy(io.MultiWriter(tempFile, hasher), os.Stdin)
	if err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	contentHash := hex.EncodeToString(hasher.Sum(nil))

	begin, err := wsCall(socketPath, wsproto.Request{Op: "writeBegin", SessionID: sessionID, AgentSelector: selector, Path: path, SizeBytes: sizeBytes})
	if err != nil {
		return err
	}
	if !begin.OK {
		return wsFailureFrom(begin)
	}

	if err := uploadToStaging(begin.URL, begin.Fields, tempFile.Name()); err != nil {
		return &wsFailure{exitCode: 1, message: err.Error()}
	}

	commit, err := wsCall(socketPath, wsproto.Request{Op: "writeCommit", SessionID: sessionID, AgentSelector: selector, Path: path, UploadToken: begin.UploadToken, ContentHash: contentHash})
	if err != nil {
		return err
	}
	if !commit.OK {
		return wsFailureFrom(commit)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", commit.DisplayPath, commit.SizeBytes)
	return nil
}

// uploadToStaging POSTs the spooled file to the presigned S3 staging URL as multipart/form-data — every policy
// field first, the file part ("file") last, exactly as a browser upload does. The body is ≤ the workspace write
// cap (10 MB), so buffering it is fine.
func uploadToStaging(url string, fields map[string]string, filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			return err
		}
	}
	part, err := writer.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, file); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	response, err := http.Post(url, writer.FormDataContentType(), body)
	if err != nil {
		return fmt.Errorf("uploading to storage: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("storage upload failed: HTTP %d", response.StatusCode)
	}
	return nil
}

func wsCall(socketPath string, request wsproto.Request) (wsproto.Response, error) {
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return wsproto.Response{}, &wsFailure{exitCode: 1, message: fmt.Sprintf("cannot reach the AgentParley daemon at %s: %v", socketPath, err)}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(40 * time.Second))

	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return wsproto.Response{}, err
	}
	var response wsproto.Response
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return wsproto.Response{}, err
	}
	return response, nil
}

// wsFailure carries the exit code a `ws` error should produce.
type wsFailure struct {
	exitCode int
	message  string
}

func (e *wsFailure) Error() string { return e.message }

func wsFailureFrom(response wsproto.Response) *wsFailure {
	exitCode := 1
	if response.ErrorCode == "file_not_found" {
		exitCode = 2
	}
	message := response.ErrorMessage
	if message == "" {
		message = response.ErrorCode
	}
	return &wsFailure{exitCode: exitCode, message: message}
}

func exitOnWsFailure(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "agentparley ws:", err.Error())
	var failure *wsFailure
	if errors.As(err, &failure) {
		os.Exit(failure.exitCode)
	}
	os.Exit(1)
}
