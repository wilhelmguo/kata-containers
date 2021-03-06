// Copyright (c) 2017-2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"sync"
	"time"

	"github.com/containerd/console"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/katautils"
	clientUtils "github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/agent/protocols/client"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
	"go.opentelemetry.io/otel/label"
)

const (

	// The buffer size used to specify the buffer for IO streams copy
	bufSize = 1024 * 2

	defaultTimeout = 3 * time.Second

	subCommandName = "exec"
	// command-line parameters name
	paramKataMonitorAddr                     = "kata-monitor-addr"
	paramDebugConsolePort                    = "kata-debug-port"
	defaultKernelParamDebugConsoleVPortValue = 1026
	defaultParamKataMonitorAddr              = "http://localhost:8090"
)

var (
	bufPool = sync.Pool{
		New: func() interface{} {
			buffer := make([]byte, bufSize)
			return &buffer
		},
	}
)

var kataExecCLICommand = cli.Command{
	Name:  subCommandName,
	Usage: "Enter into guest by debug console",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  paramKataMonitorAddr,
			Usage: "Kata monitor listen address.",
		},
		cli.Uint64Flag{
			Name:  paramDebugConsolePort,
			Usage: "Port that debug console is listening on.",
		},
	},
	Action: func(context *cli.Context) error {
		ctx, err := cliContextToContext(context)
		if err != nil {
			return err
		}
		span, _ := katautils.Trace(ctx, subCommandName)
		defer span.End()

		endPoint := context.String(paramKataMonitorAddr)
		if endPoint == "" {
			endPoint = defaultParamKataMonitorAddr
		}
		span.SetAttributes(label.Key("endPoint").String(endPoint))

		port := context.Uint64(paramDebugConsolePort)
		if port == 0 {
			port = defaultKernelParamDebugConsoleVPortValue
		}
		span.SetAttributes(label.Key("port").Uint64(port))

		sandboxID := context.Args().Get(0)
		if sandboxID == "" {
			return fmt.Errorf("SandboxID not found")
		}
		span.SetAttributes(label.Key("sandbox").String(sandboxID))

		conn, err := getConn(endPoint, sandboxID, port)
		if err != nil {
			return err
		}
		defer conn.Close()

		con := console.Current()
		defer con.Reset()

		if err := con.SetRaw(); err != nil {
			return err
		}

		iostream := &iostream{
			conn:   conn,
			exitch: make(chan struct{}),
			closed: false,
		}

		ioCopy(iostream, con)

		<-iostream.exitch
		return nil
	},
}

func ioCopy(stream *iostream, con console.Console) {
	var wg sync.WaitGroup

	// stdin
	go func() {
		p := bufPool.Get().(*[]byte)
		defer bufPool.Put(p)
		io.CopyBuffer(stream, con, *p)
	}()

	// stdout
	wg.Add(1)
	go func() {
		p := bufPool.Get().(*[]byte)
		defer bufPool.Put(p)
		io.CopyBuffer(os.Stdout, stream, *p)
		wg.Done()
	}()

	wg.Wait()
	close(stream.exitch)
}

type iostream struct {
	conn   net.Conn
	exitch chan struct{}
	closed bool
}

func (s *iostream) Write(data []byte) (n int, err error) {
	if s.closed {
		return 0, errors.New("stream closed")
	}
	return s.conn.Write(data)
}

func (s *iostream) Close() error {
	if s.closed {
		return errors.New("stream closed")
	}

	err := s.conn.Close()
	if err == nil {
		s.closed = true
	}

	return err
}

func (s *iostream) Read(data []byte) (n int, err error) {
	if s.closed {
		return 0, errors.New("stream closed")
	}

	return s.conn.Read(data)
}

func getConn(endPoint, sandboxID string, port uint64) (net.Conn, error) {
	shimURL := fmt.Sprintf("%s/agent-url?sandbox=%s", endPoint, sandboxID)
	resp, err := http.Get(shimURL)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Failed to get %s: %d", shimURL, resp.StatusCode)
	}

	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	sock := strings.TrimSuffix(string(data), "\n")
	addr, err := url.Parse(sock)
	if err != nil {
		return nil, err
	}

	// validate more
	switch addr.Scheme {
	case clientUtils.VSockSocketScheme:
		// vsock://31513974:1024
		cidAndPort := strings.Split(addr.Host, ":")
		if len(cidAndPort) != 2 {
			return nil, fmt.Errorf("Invalid vsock scheme: %s", sock)
		}
		shimAddr := fmt.Sprintf("%s:%s:%d", clientUtils.VSockSocketScheme, cidAndPort[0], port)
		return clientUtils.VsockDialer(shimAddr, defaultTimeout)

	case clientUtils.HybridVSockScheme:
		// addr: hvsock:///run/vc/firecracker/340b412c97bf1375cdda56bfa8f18c8a/root/kata.hvsock:1024
		hvsocket := strings.Split(addr.Path, ":")
		if len(hvsocket) != 2 {
			return nil, fmt.Errorf("Invalid hybrid vsock scheme: %s", sock)
		}

		// hvsock:///run/vc/firecracker/340b412c97bf1375cdda56bfa8f18c8a/root/kata.hvsock
		shimAddr := fmt.Sprintf("%s:%s:%d", clientUtils.HybridVSockScheme, hvsocket[0], port)
		return clientUtils.HybridVSockDialer(shimAddr, defaultTimeout)
	}

	return nil, fmt.Errorf("schema %s not found", addr.Scheme)
}
