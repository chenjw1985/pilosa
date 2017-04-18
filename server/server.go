// package server contains the `pilosa server` subcommand which runs Pilosa
// itself. The purpose of this package is to define an easily tested Command
// object which handles interpreting configuration and setting up all the
// objects that Pilosa needs.
package server

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pilosa/pilosa"
	"github.com/pilosa/pilosa/gossip"
	"github.com/pilosa/pilosa/httpbroadcast"
)

func init() {
	rand.Seed(time.Now().UTC().UnixNano())
}

const (
	// DefaultDataDir is the default data directory.
	DefaultDataDir = "~/.pilosa"
)

// Command represents the state of the pilosa server command.
type Command struct {
	Server *pilosa.Server

	// Configuration.
	Config *pilosa.Config

	// Profiling options.
	CPUProfile string
	CPUTime    time.Duration

	// Standard input/output
	*pilosa.CmdIO

	// running will be closed once Command.Run is finished.
	Started chan struct{}
	// Done will be closed when Command.Close() is called
	Done chan struct{}
}

// NewMain returns a new instance of Main.
func NewCommand(stdin io.Reader, stdout, stderr io.Writer) *Command {
	return &Command{
		Server: pilosa.NewServer(),
		Config: pilosa.NewConfig(),

		CmdIO: pilosa.NewCmdIO(stdin, stdout, stderr),

		Started: make(chan struct{}),
		Done:    make(chan struct{}),
	}
}

// Run executes the pilosa server.
func (m *Command) Run(args ...string) (err error) {
	defer close(m.Started)
	prefix := "~" + string(filepath.Separator)
	if strings.HasPrefix(m.Config.DataDir, prefix) {
		HomeDir := os.Getenv("HOME")
		if HomeDir == "" {
			return errors.New("data directory not specified and no home dir available")
		}
		m.Config.DataDir = filepath.Join(HomeDir, strings.TrimPrefix(m.Config.DataDir, prefix))
	}

	// SetupServer
	err = m.SetupServer()
	if err != nil {
		return err
	}

	// Initialize server.
	if err = m.Server.Open(); err != nil {
		return fmt.Errorf("server.Open: %v", err)
	}
	fmt.Fprintf(m.Stderr, "Listening as http://%s\n", m.Server.Host)
	return nil
}

func (m *Command) SetupServer() error {
	cluster := pilosa.NewCluster()
	cluster.ReplicaN = m.Config.Cluster.ReplicaN

	for _, hostport := range m.Config.Cluster.Nodes {
		cluster.Nodes = append(cluster.Nodes, &pilosa.Node{Host: hostport})
	}
	m.Server.Cluster = cluster

	// Setup logging output.
	if m.Config.LogPath == "" {
		m.Server.LogOutput = m.Stderr
	} else {
		logFile, err := os.OpenFile(m.Config.LogPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		m.Server.LogOutput = logFile
	}

	// Configure index.
	fmt.Fprintf(m.Stderr, "Using data from: %s\n", m.Config.DataDir)
	m.Server.Index.Path = m.Config.DataDir
	m.Server.Index.Stats = pilosa.NewExpvarStatsClient()

	var err error
	m.Server.Host, err = normalizeHost(m.Config.Host)
	if err != nil {
		return err
	}

	switch m.Config.Cluster.Type { // TODO change name to something that encompasses broadcasting, receiving broadcasts, and tracking cluster membership
	case "http":
		m.Server.Broadcaster = httpbroadcast.NewHTTPBroadcaster(m.Server, m.Config.Cluster.InternalPort)
		m.Server.BroadcastReceiver = httpbroadcast.NewHTTPBroadcastReceiver(m.Config.Cluster.InternalPort, m.Stderr)
		m.Server.Cluster.NodeSet = httpbroadcast.NewHTTPNodeSet()
		err := m.Server.Cluster.NodeSet.(*httpbroadcast.HTTPNodeSet).Join(m.Server.Cluster.Nodes)
		if err != nil {
			return err
		}
	case "gossip":
		gossipPortStr := pilosa.DefaultGossipPort
		if m.Config.Cluster.InternalPort != "" {
			gossipPortStr = m.Config.Cluster.InternalPort
		}
		gossipPort, err := strconv.Atoi(gossipPortStr)
		if err != nil {
			return err
		}
		gossipSeed := pilosa.DefaultHost
		if m.Config.Cluster.GossipSeed != "" {
			gossipSeed = m.Config.Cluster.GossipSeed
		}
		// get the host portion of addr to use for binding
		gossipHost, _, err := net.SplitHostPort(m.Config.Host)
		if err != nil {
			gossipHost = m.Config.Host
		}
		gossipNodeSet := gossip.NewGossipNodeSet(m.Config.Host, gossipHost, gossipPort, gossipSeed, m.Server)
		m.Server.Cluster.NodeSet = gossipNodeSet
		m.Server.Broadcaster = gossipNodeSet
		m.Server.BroadcastReceiver = gossipNodeSet
	case "static", "":
		m.Server.Broadcaster = pilosa.NopBroadcaster
		m.Server.Cluster.NodeSet = pilosa.NewStaticNodeSet()
		m.Server.BroadcastReceiver = pilosa.NopBroadcastReceiver
	default:
		return fmt.Errorf("'%v' is not a supported value for broadcaster type.", m.Config.Cluster.Type)
	}

	// Set configuration options.
	m.Server.AntiEntropyInterval = time.Duration(m.Config.AntiEntropy.Interval)
	return nil
}

func normalizeHost(host string) (string, error) {
	if !strings.Contains(host, ":") {
		host = host + ":"
	} else if strings.Contains(host, "://") {
		if strings.HasPrefix(host, "http://") {
			host = host[7:]
		} else {
			return "", fmt.Errorf("invalid scheme or host: '%s'. use the format [http://]<host>:<port>", host)
		}
	}
	return host, nil
}

// Close shuts down the server.
func (m *Command) Close() error {
	var logErr error
	serveErr := m.Server.Close()
	logOutput := m.Server.LogOutput
	if closer, ok := logOutput.(io.Closer); ok {
		logErr = closer.Close()
	}
	close(m.Done)
	if serveErr != nil && logErr != nil {
		return fmt.Errorf("closing server: '%v', closing logs: '%v'", serveErr, logErr)
	} else if logErr != nil {
		return logErr
	}
	return serveErr
}
