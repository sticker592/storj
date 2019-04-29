// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/zeebo/errs"
	"golang.org/x/sync/errgroup"

	"storj.io/storj/internal/fpath"
	"storj.io/storj/internal/processgroup"
)

const (
	maxInstanceCount    = 100
	maxStoragenodeCount = 200

	folderPermissions = 0744
)

const (
	// The following values of peer class and endpoints are used
	// to create a port with a consistent format for storj-sim services.

	// Peer class
	satellitePeer      = 0
	gatewayPeer        = 1
	versioncontrolPeer = 2
	bootstrapPeer      = 3
	storagenodePeer    = 4

	// Endpoint
	publicGRPC  = 0
	privateGRPC = 1
	publicHTTP  = 2
	debugHTTP   = 9
)

// port creates a port with a consistent format for storj-sim services.
// The port format is: "1PXXE", where P is the peer class, XX is the index of the instance, and E is the endpoint.
func port(peerclass, index, endpoint int) string {
	port := 10000 + peerclass*1000 + index*10 + endpoint
	return strconv.Itoa(port)
}

func networkExec(flags *Flags, args []string, command string) error {
	processes, err := newNetwork(flags)
	if err != nil {
		return err
	}

	ctx, cancel := NewCLIContext(context.Background())
	defer cancel()

	if command == "setup" {
		identities, err := identitySetup(processes)
		if err != nil {
			return err
		}

		err = identities.Exec(ctx, command)
		if err != nil {
			return err
		}
	}

	err = processes.Exec(ctx, command)
	closeErr := processes.Close()

	return errs.Combine(err, closeErr)
}

func networkEnv(flags *Flags, args []string) error {
	flags.OnlyEnv = true
	processes, err := newNetwork(flags)
	if err != nil {
		return err
	}

	// run exec before, since it will load env vars from configs
	for _, process := range processes.List {
		if exec := process.ExecBefore["run"]; exec != nil {
			if err := exec(process); err != nil {
				return err
			}
		}
	}

	if len(args) == 1 {
		envprefix := strings.ToUpper(args[0] + "=")
		// find the environment value that the environment variable is set to
		for _, env := range processes.Env() {
			if strings.HasPrefix(strings.ToUpper(env), envprefix) {
				fmt.Println(env[len(envprefix):])
				return nil
			}
		}

		return nil
	}

	for _, env := range processes.Env() {
		fmt.Println(env)
	}

	return nil
}

func networkTest(flags *Flags, command string, args []string) error {
	processes, err := newNetwork(flags)
	if err != nil {
		return err
	}

	ctx, cancel := NewCLIContext(context.Background())

	var group errgroup.Group
	processes.Start(ctx, &group, "run")

	for _, process := range processes.List {
		process.Status.Started.Wait()
	}

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = append(os.Environ(), processes.Env()...)
	stdout := processes.Output.Prefixed("test:out")
	stderr := processes.Output.Prefixed("test:err")
	cmd.Stdout, cmd.Stderr = stdout, stderr
	processgroup.Setup(cmd)

	if printCommands {
		fmt.Fprintf(processes.Output, "exec: %v\n", strings.Join(cmd.Args, " "))
	}
	errRun := cmd.Run()

	cancel()
	return errs.Combine(errRun, processes.Close(), group.Wait())
}

func networkDestroy(flags *Flags, args []string) error {
	if fpath.IsRoot(flags.Directory) {
		return errors.New("safety check: disallowed to remove root directory " + flags.Directory)
	}
	if printCommands {
		fmt.Println("sim | exec: rm -rf", flags.Directory)
	}
	return os.RemoveAll(flags.Directory)
}

// newNetwork creates a default network
func newNetwork(flags *Flags) (*Processes, error) {
	// with common adds all common arguments to the process
	withCommon := func(dir string, all Arguments) Arguments {
		common := []string{"--metrics.app-suffix", "sim", "--log.level", "debug", "--config-dir", dir}
		if flags.IsDev {
			common = append(common, "--dev")
		}
		for command, args := range all {
			all[command] = append(append(common, command), args...)
		}
		return all
	}

	processes := NewProcesses(flags.Directory)

	var host = flags.Host
	versioncontrol := processes.New(Info{
		Name:       "versioncontrol/0",
		Executable: "versioncontrol",
		Directory:  filepath.Join(processes.Directory, "versioncontrol", "0"),
		Address:    net.JoinHostPort(host, port(versioncontrolPeer, 0, publicGRPC)),
	})

	versioncontrol.Arguments = withCommon(versioncontrol.Directory, Arguments{
		"setup": {
			"--address", versioncontrol.Address,
			"--debug.addr", net.JoinHostPort("127.0.0.1", port(versioncontrolPeer, 0, debugHTTP)),
		},
		"run": {},
	})

	versioncontrol.ExecBefore["run"] = func(process *Process) error {
		return readConfigString(&versioncontrol.Address, versioncontrol.Directory, "address")
	}

	bootstrap := processes.New(Info{
		Name:       "bootstrap/0",
		Executable: "bootstrap",
		Directory:  filepath.Join(processes.Directory, "bootstrap", "0"),
		Address:    net.JoinHostPort(host, port(bootstrapPeer, 0, publicGRPC)),
	})

	// gateway must wait for the versioncontrol to start up
	bootstrap.WaitForStart(versioncontrol)

	bootstrap.Arguments = withCommon(bootstrap.Directory, Arguments{
		"setup": {
			"--identity-dir", bootstrap.Directory,

			"--web.address", net.JoinHostPort(host, port(bootstrapPeer, 0, publicHTTP)),

			"--server.address", bootstrap.Address,
			"--server.private-address", net.JoinHostPort(host, port(bootstrapPeer, 0, privateGRPC)),

			"--kademlia.bootstrap-addr", bootstrap.Address,
			"--kademlia.operator.email", "bootstrap@example.com",
			"--kademlia.operator.wallet", "0x0123456789012345678901234567890123456789",

			"--server.extensions.revocation=false",
			"--server.use-peer-ca-whitelist=false",

			"--version.server-address", fmt.Sprintf("http://%s/", versioncontrol.Address),

			"--debug.addr", net.JoinHostPort("127.0.0.1", port(bootstrapPeer, 0, debugHTTP)),
		},
		"run": {},
	})
	bootstrap.ExecBefore["run"] = func(process *Process) error {
		return readConfigString(&bootstrap.Address, bootstrap.Directory, "server.address")
	}

	// Create satellites making all satellites wait for bootstrap to start
	if flags.SatelliteCount > maxInstanceCount {
		return nil, fmt.Errorf("exceeded the max instance count of %d with Satellite count of %d", maxInstanceCount, flags.SatelliteCount)
	}

	var satellites []*Process
	for i := 0; i < flags.SatelliteCount; i++ {
		process := processes.New(Info{
			Name:       fmt.Sprintf("satellite/%d", i),
			Executable: "satellite",
			Directory:  filepath.Join(processes.Directory, "satellite", fmt.Sprint(i)),
			Address:    net.JoinHostPort(host, port(satellitePeer, i, publicGRPC)),
		})
		satellites = append(satellites, process)

		// satellite must wait for bootstrap to start
		process.WaitForStart(bootstrap)

		// TODO: find source file, to set static path
		_, filename, _, ok := runtime.Caller(0)
		if !ok {
			return nil, errs.Combine(processes.Close(), errs.New("no caller information"))
		}
		storjRoot := strings.TrimSuffix(filename, "/cmd/storj-sim/network.go")

		consoleAuthToken := "secure_token"

		process.Arguments = withCommon(process.Directory, Arguments{
			"setup": {
				"--identity-dir", process.Directory,
				"--console.address", net.JoinHostPort(host, port(satellitePeer, i, publicHTTP)),
				"--console.static-dir", filepath.Join(storjRoot, "web/satellite/"),
				// TODO: remove console.auth-token after vanguard release
				"--console.auth-token", consoleAuthToken,
				"--server.address", process.Address,
				"--server.private-address", net.JoinHostPort(host, port(satellitePeer, i, privateGRPC)),

				"--kademlia.bootstrap-addr", bootstrap.Address,

				"--server.extensions.revocation=false",
				"--server.use-peer-ca-whitelist=false",

				"--mail.smtp-server-address", "smtp.gmail.com:587",
				"--mail.from", "Storj <yaroslav-satellite-test@storj.io>",
				"--mail.template-path", filepath.Join(storjRoot, "web/satellite/static/emails"),

				"--version.server-address", fmt.Sprintf("http://%s/", versioncontrol.Address),
				"--debug.addr", net.JoinHostPort("127.0.0.1", port(satellitePeer, i, debugHTTP)),
			},
			"run": {},
		})

		process.ExecBefore["run"] = func(process *Process) error {
			return readConfigString(&process.Address, process.Directory, "server.address")
		}
	}

	// Create gateways for each satellite
	for i, satellite := range satellites {
		satellite := satellite
		process := processes.New(Info{
			Name:       fmt.Sprintf("gateway/%d", i),
			Executable: "gateway",
			Directory:  filepath.Join(processes.Directory, "gateway", fmt.Sprint(i)),
			Address:    net.JoinHostPort(host, port(gatewayPeer, i, publicGRPC)),
			Extra:      []string{},
		})

		// gateway must wait for the corresponding satellite to start up
		process.WaitForStart(satellite)
		process.Arguments = withCommon(process.Directory, Arguments{
			"setup": {
				"--identity-dir", process.Directory,
				"--satellite-addr", satellite.Address,

				"--server.address", process.Address,

				"--satellite-addr", satellite.Address,

				"--rs.min-threshold", strconv.Itoa(1 * flags.StorageNodeCount / 5),
				"--rs.repair-threshold", strconv.Itoa(2 * flags.StorageNodeCount / 5),
				"--rs.success-threshold", strconv.Itoa(3 * flags.StorageNodeCount / 5),
				"--rs.max-threshold", strconv.Itoa(4 * flags.StorageNodeCount / 5),

				"--tls.extensions.revocation=false",
				"--tls.use-peer-ca-whitelist=false",

				"--debug.addr", net.JoinHostPort(host, port(gatewayPeer, i, debugHTTP)),
			},
			"run": {
				"--encryption-key=TestEncryptionKey",
			},
		})

		process.ExecBefore["run"] = func(process *Process) error {
			err := readConfigString(&process.Address, process.Directory, "server.address")
			if err != nil {
				return err
			}

			vip := viper.New()
			vip.AddConfigPath(process.Directory)
			if err := vip.ReadInConfig(); err != nil {
				return err
			}

			// TODO: maybe all the config flags should be exposed for all processes?

			// check if gateway config has an api key, if it's not
			// create example project with key and add it to the config
			// so that gateway can have access to the satellite
			apiKey := vip.GetString("api-key")
			if !flags.OnlyEnv && apiKey == "" {
				var consoleAddress string
				satelliteConfigErr := readConfigString(&consoleAddress, satellite.Directory, "console.address")
				if satelliteConfigErr != nil {
					return satelliteConfigErr
				}

				host := "http://" + consoleAddress
				createRegistrationTokenAddress := host + "/registrationToken/?projectsLimit=1"
				consoleActivationAddress := host + "/activation/?token="
				consoleAPIAddress := host + "/api/graphql/v0"

				// wait for console server to start
				time.Sleep(3 * time.Second)

				if err := addExampleProjectWithKey(&apiKey, createRegistrationTokenAddress, consoleActivationAddress, consoleAPIAddress); err != nil {
					return err
				}

				vip.Set("api-key", apiKey)

				if err := vip.WriteConfig(); err != nil {
					return err
				}
			}

			if apiKey != "" {
				process.Extra = append(process.Extra, "API_KEY="+apiKey)
			}

			accessKey := vip.GetString("minio.access-key")
			secretKey := vip.GetString("minio.secret-key")

			process.Extra = append(process.Extra,
				"ACCESS_KEY="+accessKey,
				"SECRET_KEY="+secretKey,
			)

			return nil
		}
	}

	// Create storage nodes
	if flags.StorageNodeCount > maxStoragenodeCount {
		return nil, fmt.Errorf("exceeded the max instance count of %d with Storage Node count of %d", maxStoragenodeCount, flags.StorageNodeCount)
	}
	for i := 0; i < flags.StorageNodeCount; i++ {
		process := processes.New(Info{
			Name:       fmt.Sprintf("storagenode/%d", i),
			Executable: "storagenode",
			Directory:  filepath.Join(processes.Directory, "storagenode", fmt.Sprint(i)),
			Address:    net.JoinHostPort(host, port(storagenodePeer, i, publicGRPC)),
		})

		// storage node must wait for bootstrap and satellites to start
		process.WaitForStart(bootstrap)
		for _, satellite := range satellites {
			process.WaitForStart(satellite)
		}

		process.Arguments = withCommon(process.Directory, Arguments{
			"setup": {
				"--identity-dir", process.Directory,
				"--server.address", process.Address,
				"--server.private-address", net.JoinHostPort(host, port(storagenodePeer, i, privateGRPC)),

				"--kademlia.bootstrap-addr", bootstrap.Address,
				"--kademlia.operator.email", fmt.Sprintf("storage%d@example.com", i),
				"--kademlia.operator.wallet", "0x0123456789012345678901234567890123456789",

				"--server.extensions.revocation=false",
				"--server.use-peer-ca-whitelist=false",
				"--storage.satellite-id-restriction=false",

				"--version.server-address", fmt.Sprintf("http://%s/", versioncontrol.Address),
				"--debug.addr", net.JoinHostPort("127.0.0.1", port(storagenodePeer, i, debugHTTP)),
			},
			"run": {},
		})

		process.ExecBefore["run"] = func(process *Process) error {
			return readConfigString(&process.Address, process.Directory, "server.address")
		}
	}

	{ // verify that we have all binaries
		missing := map[string]bool{}
		for _, process := range processes.List {
			_, err := exec.LookPath(process.Executable)
			if err != nil {
				missing[process.Executable] = true
			}
		}
		if len(missing) > 0 {
			var list []string
			for executable := range missing {
				list = append(list, executable)
			}
			sort.Strings(list)
			return nil, fmt.Errorf("some executables cannot be found: %v", list)
		}
	}

	// Create directories for all processes
	for _, process := range processes.List {
		if err := os.MkdirAll(process.Directory, folderPermissions); err != nil {
			return nil, err
		}
	}

	return processes, nil
}

func identitySetup(network *Processes) (*Processes, error) {
	processes := NewProcesses(network.Directory)

	for _, process := range network.List {
		identity := processes.New(Info{
			Name:       "identity/" + process.Info.Name,
			Executable: "identity",
			Directory:  process.Directory,
			Address:    "",
		})

		identity.Arguments = Arguments{
			"setup": {
				"--identity-dir", process.Directory,
				"--concurrency", "1",
				"--difficulty", "8",
				"create", ".",
			},
		}
	}

	// create directories for all processes
	for _, process := range processes.List {
		if err := os.MkdirAll(process.Directory, folderPermissions); err != nil {
			return nil, err
		}
	}

	return processes, nil
}

// readConfigString reads from dir/config.yaml flagName returns the value in `into`
func readConfigString(into *string, dir, flagName string) error {
	vip := viper.New()
	vip.AddConfigPath(dir)
	if err := vip.ReadInConfig(); err != nil {
		return err
	}
	if v := vip.GetString(flagName); v != "" {
		*into = v
	}
	return nil
}
