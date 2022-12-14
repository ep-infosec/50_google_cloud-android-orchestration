// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cli

import (
	"fmt"
	"net"
	"os/exec"
	"sync/atomic"

	wclient "github.com/google/cloud-android-orchestration/pkg/webrtcclient"

	"github.com/hashicorp/go-multierror"
	"github.com/pion/webrtc/v3"
	"github.com/spf13/cobra"
)

const connectFlag = "connect"

type adbTunnelFlags struct {
	*subCommandFlags
	host string
}

type openADBTunnelFlags struct {
	*adbTunnelFlags
	connect bool
}

func newADBTunnelCommand(cfgFlags *configFlags) *cobra.Command {
	adbTunnelFlags := &adbTunnelFlags{&subCommandFlags{cfgFlags, false}, ""}
	openFlags := &openADBTunnelFlags{adbTunnelFlags, false}
	open := &cobra.Command{
		Use:   "open",
		Short: "Opens an ADB tunnel.",
		RunE: func(c *cobra.Command, args []string) error {
			return runOpenADBTunnelCommand(openFlags, &command{c, &adbTunnelFlags.Verbose}, args)
		},
	}
	open.PersistentFlags().BoolVarP(
		&openFlags.connect, connectFlag,
		"c",
		false,
		"Issue the `adb connect` command after the tunnel is open")
	list := &cobra.Command{
		Use:   "list",
		Short: "Lists open ADB tunnels.",
		RunE:  notImplementedCommand,
	}
	close := &cobra.Command{
		Use:   "close <foo> <bar> <baz>",
		Short: "Close ADB tunnels.",
		RunE:  notImplementedCommand,
	}
	adbTunnel := &cobra.Command{
		Use:   "adbtunnel",
		Short: "Work with ADB tunnels",
	}
	addCommonSubcommandFlags(adbTunnel, adbTunnelFlags.subCommandFlags)
	adbTunnel.PersistentFlags().StringVar(&adbTunnelFlags.host, hostFlag, "", "Specifies the host")
	adbTunnel.AddCommand(open)
	adbTunnel.AddCommand(list)
	adbTunnel.AddCommand(close)
	return adbTunnel
}

func runOpenADBTunnelCommand(flags *openADBTunnelFlags, c *command, args []string) error {
	adbPath := ""
	if flags.connect {
		path, err := exec.LookPath("adb")
		if err != nil {
			return fmt.Errorf("Can't connect adb: %w", err)
		}
		adbPath = path
	}
	apiClient, err := buildAPIClient(flags.subCommandFlags, c.Command)
	if err != nil {
		return err
	}

	var merr error
	var conns []*wclient.Connection

	for _, device := range args {
		observer := ADBForwarder{
			cmd:     c,
			device:  device,
			adbPath: adbPath,
		}
		conn, err := apiClient.ConnectWebRTC(flags.host, device, &observer)
		if err != nil {
			err = fmt.Errorf("ADB tunnel creation failed for %q: %w", device, err)
			c.PrintErrln(err)
			merr = multierror.Append(merr, err)
		} else {
			// TODO(jemoreira): close everything except the ADB data channel.
			conns = append(conns, conn)
		}
	}

	if len(conns) == 0 {
		// Return if no tunnels were successfully created
		return merr
	}

	// Wait forever
	select {}
}

// Forwards ADB messages between a local ADB server and a remote CVD.
// Implements the Observer interface for the webrtc client.
type ADBForwarder struct {
	cmd      *command
	device   string
	adbPath  string
	dc       *webrtc.DataChannel
	listener net.Listener
	conn     net.Conn
	port     int
	running  atomic.Bool
}

func (f *ADBForwarder) OnADBDataChannel(dc *webrtc.DataChannel) {
	f.dc = dc
	f.cmd.PrintVerbosef("ADB data channel to %q changed state: %v\n", f.device, dc.ReadyState())
	// Accept connections on the port only after the device is ready to accept messages
	dc.OnOpen(func() {
		if err := f.StartForwarding(); err != nil {
			f.cmd.PrintErrf("Failed to bind to local port for %q: %v", f.device, err)
			return
		}
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if err := f.Send(msg.Data); err != nil {
				f.cmd.PrintErrf("Error writing to ADB server for %q: %v", f.device, err)
			}
		})
		dc.OnClose(func() {
			f.StopForwarding()
		})
		// Connect after OnMessage and OnClose are setup, otherwise it will timeout.
		adbSerial := fmt.Sprintf("127.0.0.1:%d", f.port)
		if f.adbPath != "" {
			cmd := exec.Command(f.adbPath, "connect", adbSerial)
			// Make sure any adb errors are printed. Don't do the same for stdout: we'll print a
			// similar message with the device name.
			cmd.Stderr = f.cmd.ErrOrStderr()
			if err := cmd.Run(); err != nil {
				f.cmd.PrintErrf("Error attempting to connect adb to %q: %v", f.device, err)
				return
			}
			f.cmd.PrintErrf("%s connected on: %s\n", f.device, adbSerial)
		} else {
			// Print the address to stdout if adb wasn't connected automatically
			f.cmd.Printf("%s: %s\n", f.device, adbSerial)
		}
	})
}

func (f *ADBForwarder) OnError(err error) {
	f.cmd.PrintErrf("Error on webrtc connection to %q: %v\n", f.device, err)
}

func (f *ADBForwarder) OnFailure() {
	f.cmd.PrintVerbosef("WebRTC connection to %q set to failed state", f.device)
}

func (f *ADBForwarder) OnClose() {
	f.cmd.PrintVerbosef("WebRTC connection to %q closed", f.device)
}

func (f *ADBForwarder) StartForwarding() error {
	listener, err := net.Listen("tcp", "127.0.0.1:")
	if err != nil {
		return err
	}
	f.listener = listener
	f.running.Store(true)
	f.port = listener.Addr().(*net.TCPAddr).Port
	go f.acceptLoop(listener)
	return nil
}

func (f *ADBForwarder) StopForwarding() {
	// Set running to false, return immediately if it already was false.
	if !f.running.Swap(false) {
		return
	}
	// Prevent future writes to the channel too.
	f.dc.Close()
	// f.listener is guaranteed to be non-nil at this point
	f.listener.Close()
	if f.conn != nil {
		f.conn.Close()
	}
}

func (f *ADBForwarder) Send(data []byte) error {
	if f.conn == nil {
		return fmt.Errorf("No connection yet on port %d", f.port)
	}
	// Once f.conn is not nil it's safe to use. The worst that can happen is that
	// we write to a closed connection which simply returns an error.
	length := 0
	for length < len(data) {
		l, err := f.conn.Write(data[length:])
		if err != nil {
			return err
		}
		length += l
	}
	return nil
}

func (f *ADBForwarder) acceptLoop(listener net.Listener) {
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if !f.running.Load() {
			// StopForwarding was called, Accept likely returned an error because
			// listener was closed, ignore it.
			return
		}
		f.conn = conn
		if err != nil {
			f.cmd.PrintErrf("Error accepting connection on port %d: %v", f.port, err)
			return
		}
		f.cmd.PrintVerbosef("Connection received on port %d", f.port)
		if err := f.recvLoop(); err != nil {
			f.cmd.PrintErrln(err)
			return
		}
	}
}

func (f *ADBForwarder) recvLoop() error {
	defer f.conn.Close()
	var buffer [4096]byte
	for {
		length, err := f.conn.Read(buffer[:])
		if !f.running.Load() {
			// StopForwarding was called, Read likely returned an error because
			// conn was closed, ignore it.
			return nil
		}
		if err != nil {
			return fmt.Errorf("Error receiving from port %d: %v", f.port, err)
		}
		if length == 0 {
			// Connection closed
			return nil
		}
		err = f.dc.Send(buffer[:length])
		if err != nil {
			return fmt.Errorf("Failed to send ADB data to %q: %v", f.device, err)
		}
	}
}
