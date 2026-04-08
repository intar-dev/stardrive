package sshutil

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type Credentials struct {
	User           string
	Password       string
	PrivateKeyPEM  []byte
	ConnectTimeout time.Duration
}

type Client struct {
	raw *ssh.Client
}

func Dial(ctx context.Context, address string, creds Credentials) (*Client, error) {
	user := strings.TrimSpace(creds.User)
	if user == "" {
		user = "root"
	}
	authMethods := make([]ssh.AuthMethod, 0, 2)
	if len(bytes.TrimSpace(creds.PrivateKeyPEM)) > 0 {
		signer, err := ssh.ParsePrivateKey(creds.PrivateKeyPEM)
		if err != nil {
			return nil, fmt.Errorf("parse SSH private key: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}
	if strings.TrimSpace(creds.Password) != "" {
		authMethods = append(authMethods, ssh.Password(strings.TrimSpace(creds.Password)))
	}
	if len(authMethods) == 0 {
		return nil, fmt.Errorf("at least one SSH auth method is required")
	}

	sshConfig := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         creds.ConnectTimeout,
	}
	if sshConfig.Timeout <= 0 {
		sshConfig.Timeout = 10 * time.Second
	}

	dialer := &net.Dialer{Timeout: sshConfig.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", strings.TrimSpace(address))
	if err != nil {
		return nil, fmt.Errorf("dial SSH %s: %w", address, err)
	}

	clientConn, chans, reqs, err := ssh.NewClientConn(conn, strings.TrimSpace(address), sshConfig)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("establish SSH session to %s: %w", address, err)
	}

	return &Client{raw: ssh.NewClient(clientConn, chans, reqs)}, nil
}

func (c *Client) Close() error {
	if c == nil || c.raw == nil {
		return nil
	}
	return c.raw.Close()
}

func (c *Client) RunScript(ctx context.Context, script string) (string, string, error) {
	if c == nil || c.raw == nil {
		return "", "", fmt.Errorf("SSH client is not connected")
	}

	session, err := c.raw.NewSession()
	if err != nil {
		return "", "", fmt.Errorf("create SSH session: %w", err)
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return "", "", fmt.Errorf("open SSH stdin: %w", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	if err := session.Start("/bin/sh -se"); err != nil {
		stdin.Close()
		return "", "", fmt.Errorf("start remote shell: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- session.Wait()
	}()
	if _, err := stdin.Write([]byte(script)); err != nil {
		_ = stdin.Close()
		_ = session.Close()
		<-done
		return stdout.String(), stderr.String(), fmt.Errorf("write SSH script: %w", err)
	}
	if err := stdin.Close(); err != nil {
		_ = session.Close()
		<-done
		return stdout.String(), stderr.String(), fmt.Errorf("close SSH stdin: %w", err)
	}

	select {
	case <-ctx.Done():
		_ = session.Close()
		<-done
		return stdout.String(), stderr.String(), ctx.Err()
	case err := <-done:
		if err != nil {
			return stdout.String(), stderr.String(), fmt.Errorf("run remote script: %w", err)
		}
		return stdout.String(), stderr.String(), nil
	}
}
