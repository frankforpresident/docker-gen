package generator

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/nginx-proxy/docker-gen/internal/config"
	"github.com/nginx-proxy/docker-gen/internal/context"
	"github.com/nginx-proxy/docker-gen/internal/dockerclient"
	"github.com/nginx-proxy/docker-gen/internal/template"
	"github.com/nginx-proxy/docker-gen/internal/utils"
)

// Helper function to safely get ID prefix (first 12 chars) or the full ID if shorter
func getIDPrefix(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Helper function to safely get container health status
func getContainerHealthStatus(container *docker.Container) string {
	// The docker.Container struct's State.Health field might be zero-initialized
	// We need to check if it's usable by using a defer/recover pattern
	if container == nil {
		return ""
	}
	
	// Use a function with defer/recover to safely access potentially nil fields
	var status string
	func() {
		defer func() {
			// Recover from any panic that might occur when accessing nil fields
			if r := recover(); r != nil {
				status = ""
			}
		}()
		status = container.State.Health.Status
	}()
	
	return status
}

type generator struct {
	Client                     *docker.Client
	SwarmClient                *docker.Client
	Configs                    config.ConfigFile
	Endpoint                   string
	TLSVerify                  bool
	TLSCert, TLSCaCert, TLSKey string
	All                        bool
	EventFilter                map[string][]string
	SwarmEndpoint              string

	wg    sync.WaitGroup
	retry bool
}

type GeneratorConfig struct {
	Endpoint string

	TLSCert   string
	TLSKey    string
	TLSCACert string
	TLSVerify bool
	All       bool

	EventFilter map[string][]string

	ConfigFile config.ConfigFile
	SwarmEndpoint string
}

func NewGenerator(gc GeneratorConfig) (*generator, error) {
	endpoint, err := dockerclient.GetEndpoint(gc.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("bad endpoint: %s", err)
	}

	client, err := dockerclient.NewDockerClient(endpoint, gc.TLSVerify, gc.TLSCert, gc.TLSCACert, gc.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("unable to create docker client: %s", err)
	}

	apiVersion, err := client.Version()
	if err != nil {
		log.Printf("Error retrieving docker server version info: %s\n", err)
	}

	// Grab the docker daemon info once and hold onto it
	context.SetDockerEnv(apiVersion)

	g := &generator{
		Client:         client,
		SwarmClient:    nil,
		Endpoint:       gc.Endpoint,
		TLSVerify:      gc.TLSVerify,
		TLSCert:        gc.TLSCert,
		TLSCaCert:      gc.TLSCACert,
		TLSKey:         gc.TLSKey,
		All:            gc.All,
		EventFilter:    gc.EventFilter,
		Configs:        gc.ConfigFile,
		SwarmEndpoint:  gc.SwarmEndpoint,
		retry:          true,
	}
	
	// If Swarm endpoint is provided, initialize the Swarm client
	if gc.SwarmEndpoint != "" {
		swarmEndpoint, err := dockerclient.GetEndpoint(gc.SwarmEndpoint)
		if err != nil {
			return nil, fmt.Errorf("bad swarm endpoint: %s", err)
		}
		
		swarmClient, err := dockerclient.NewDockerClient(swarmEndpoint, gc.TLSVerify, gc.TLSCert, gc.TLSCACert, gc.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("unable to create docker swarm client: %s", err)
		}
		
		g.SwarmClient = swarmClient
	}
	
	return g, nil
}

func (g *generator) Generate() error {
	// If we're in Swarm mode, pre-fetch nodes information for better context
	if g.SwarmEndpoint != "" && g.SwarmClient != nil {
		go g.refreshSwarmNodeInfo()
	}
	
	g.generateFromContainers()
	g.generateAtInterval()
	g.generateFromEvents()
	g.generateFromSignals()
	g.wg.Wait()

	return nil
}

// refreshSwarmNodeInfo fetches information about all nodes in the Swarm cluster
// and logs useful diagnostic information
func (g *generator) refreshSwarmNodeInfo() {
	if g.SwarmClient == nil {
		log.Println("Swarm client not initialized, can't refresh node info")
		return
	}

	nodes, err := g.SwarmClient.ListNodes(docker.ListNodesOptions{})
	if err != nil {
		log.Printf("Error listing Swarm nodes: %s", err)
		return
	}

	log.Printf("Swarm cluster has %d nodes", len(nodes))
	for i, node := range nodes {
		hostname := "unknown"
		if node.Description.Hostname != "" {
			hostname = node.Description.Hostname
		}
		
		status := "unknown"
		if node.Status.State != "" {
			status = string(node.Status.State)
		}
		
		role := "unknown"
		if node.Spec.Role != "" {
			role = string(node.Spec.Role)
		}
		
		availability := "unknown"
		if node.Spec.Availability != "" {
			availability = string(node.Spec.Availability)
		}
		
		isManager := "false"
		if node.ManagerStatus != nil {
			isManager = "true"
		}
		
		log.Printf("Node %d: ID=%s, Hostname=%s, Role=%s, Availability=%s, Status=%s, Manager=%s", 
			i+1, getIDPrefix(node.ID), hostname, role, availability, status, isManager)
	}
}

func (g *generator) generateFromSignals() {
	var hasWatcher bool
	for _, config := range g.Configs.Config {
		if config.Watch {
			hasWatcher = true
			break
		}
	}

	// If none of the configs need to watch for events, don't watch for signals either
	if !hasWatcher {
		return
	}

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()

		sigChan, cleanup := newSignalChannel()
		defer cleanup()
		for {
			sig := <-sigChan
			log.Printf("Received signal: %s\n", sig)
			switch sig {
			case syscall.SIGHUP:
				g.generateFromContainers()
			case syscall.SIGTERM, syscall.SIGINT:
				// exit when context is done
				return
			}
		}
	}()
}

func (g *generator) generateFromContainers() {
	containers, err := g.getContainers()
	if err != nil {
		log.Printf("Error listing containers: %s\n", err)
		return
	}
	for _, config := range g.Configs.Config {
		changed := template.GenerateFile(config, containers)
		if !changed {
			log.Printf("Contents of %s did not change. Skipping notification '%s'", config.Dest, config.NotifyCmd)
			continue
		}
		g.runNotifyCmd(config)
		g.sendSignalToContainers(config)
		g.sendSignalToFilteredContainers(config)
	}
}

func (g *generator) generateAtInterval() {
	for _, cfg := range g.Configs.Config {

		if cfg.Interval == 0 {
			continue
		}

		log.Printf("Generating every %d seconds", cfg.Interval)
		g.wg.Add(1)
		ticker := time.NewTicker(time.Duration(cfg.Interval) * time.Second)
		go func(cfg config.Config) {
			defer g.wg.Done()

			sigChan, cleanup := newSignalChannel()
			defer cleanup()
			for {
				select {
				case <-ticker.C:
					containers, err := g.getContainers()
					if err != nil {
						log.Printf("Error listing containers: %s\n", err)
						continue
					}
					// ignore changed return value. always run notify command
					template.GenerateFile(cfg, containers)
					g.runNotifyCmd(cfg)
					g.sendSignalToContainers(cfg)
					g.sendSignalToFilteredContainers(cfg)
				case sig := <-sigChan:
					log.Printf("Received signal: %s\n", sig)
					switch sig {
					case syscall.SIGTERM, syscall.SIGINT:
						ticker.Stop()
						return
					}
				}
			}
		}(cfg)
	}
}

func (g *generator) generateFromEvents() {
	configs := g.Configs.FilterWatches()
	if len(configs.Config) == 0 {
		return
	}

	var watchers []chan *docker.APIEvents

	for _, cfg := range configs.Config {

		if !cfg.Watch {
			continue
		}

		g.wg.Add(1)
		watcher := make(chan *docker.APIEvents, 100)
		watchers = append(watchers, watcher)

		go func(cfg config.Config) {
			defer g.wg.Done()
			debouncedChan := newDebounceChannel(watcher, cfg.Wait)
			for range debouncedChan {
				containers, err := g.getContainers()
				if err != nil {
					log.Printf("Error listing containers: %s\n", err)
					continue
				}
				changed := template.GenerateFile(cfg, containers)
				if !changed {
					log.Printf("Contents of %s did not change. Skipping notification '%s'", cfg.Dest, cfg.NotifyCmd)
					continue
				}
				g.runNotifyCmd(cfg)
				g.sendSignalToContainers(cfg)
				g.sendSignalToFilteredContainers(cfg)
			}
		}(cfg)
	}

	// Watch regular Docker events
	g.watchDockerEvents(watchers)

	// If Swarm endpoint is configured, watch Swarm events as well
	if g.SwarmEndpoint != "" && g.SwarmClient != nil {
		g.watchSwarmEvents(watchers)
	}
}

func (g *generator) watchDockerEvents(watchers []chan *docker.APIEvents) {
	client := g.Client
	
	// maintains docker client connection and passes events to watchers
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		// channel will be closed by go-dockerclient
		eventChan := make(chan *docker.APIEvents, 100)
		sigChan, cleanup := newSignalChannel()
		defer cleanup()

		for {
			watching := false

			if client == nil {
				var err error
				endpoint, err := dockerclient.GetEndpoint(g.Endpoint)
				if err != nil {
					log.Printf("Bad endpoint: %s", err)
					time.Sleep(10 * time.Second)
					continue
				}
				client, err = dockerclient.NewDockerClient(endpoint, g.TLSVerify, g.TLSCert, g.TLSCaCert, g.TLSKey)
				if err != nil {
					log.Printf("Unable to connect to docker daemon: %s", err)
					time.Sleep(10 * time.Second)
					continue
				}
			}

			for {
				if client == nil {
					break
				}
				if !watching {
					options := docker.EventsOptions{
						Filters: g.EventFilter,
					}

					err := client.AddEventListenerWithOptions(options, eventChan)
					if err != nil && err != docker.ErrListenerAlreadyExists {
						log.Printf("Error registering docker event listener: %s", err)
						time.Sleep(10 * time.Second)
						continue
					}
					watching = true
					log.Println("Watching docker events")
					// sync all configs after resuming listener
					g.generateFromContainers()
				}
				select {
				case event, ok := <-eventChan:
					if !ok {
						log.Printf("Docker daemon connection interrupted")
						if watching {
							client.RemoveEventListener(eventChan)
							watching = false
							client = nil
						}
						if !g.retry {
							// close all watchers and exit
							for _, watcher := range watchers {
								close(watcher)
							}
							return
						}
						// recreate channel and attempt to resume
						eventChan = make(chan *docker.APIEvents, 100)
						time.Sleep(10 * time.Second)
						break
					}

					log.Printf("Received event %s for %s %s", event.Action, event.Type, getIDPrefix(event.Actor.ID))
					// fanout event to all watchers
					for _, watcher := range watchers {
						watcher <- event
					}
				case <-time.After(10 * time.Second):
					// check for docker liveness
					err := client.Ping()
					if err != nil {
						log.Printf("Unable to ping docker daemon: %s", err)
						if watching {
							client.RemoveEventListener(eventChan)
							watching = false
							client = nil
						}
					}
				case sig := <-sigChan:
					log.Printf("Received signal: %s\n", sig)
					switch sig {
					case syscall.SIGTERM, syscall.SIGINT:
						// close all watchers and exit
						for _, watcher := range watchers {
							close(watcher)
						}
						return
					}
				}
			}
		}
	}()
}

func (g *generator) watchSwarmEvents(watchers []chan *docker.APIEvents) {
	swarmClient := g.SwarmClient
	
	// maintains swarm client connection and passes events to watchers from all nodes
	// Note: Docker Swarm events can only be reliably watched from a manager node,
	// as Swarm nodes only emit events to the managers. This is why docker-gen
	// must run on a manager node to watch events from all nodes in the cluster.
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		// channel will be closed by go-dockerclient
		eventChan := make(chan *docker.APIEvents, 100)
		sigChan, cleanup := newSignalChannel()
		defer cleanup()

		for {
			watching := false

			if swarmClient == nil {
				var err error
				endpoint, err := dockerclient.GetEndpoint(g.SwarmEndpoint)
				if err != nil {
					log.Printf("Bad swarm endpoint: %s", err)
					time.Sleep(10 * time.Second)
					continue
				}
				swarmClient, err = dockerclient.NewDockerClient(endpoint, g.TLSVerify, g.TLSCert, g.TLSCaCert, g.TLSKey)
				if err != nil {
					log.Printf("Unable to connect to docker swarm: %s", err)
					time.Sleep(10 * time.Second)
					continue
				}
				g.SwarmClient = swarmClient
			}

			for {
				if swarmClient == nil {
					break
				}
				if !watching {
					// Create a copy of the event filters
					swarmFilters := make(map[string][]string)
					for k, v := range g.EventFilter {
						swarmFilters[k] = make([]string, len(v))
						copy(swarmFilters[k], v)
					}
					
					// Add the swarm=true filter to ensure we get events from all swarm nodes
					swarmFilters["swarm"] = []string{"true"}
					
					options := docker.EventsOptions{
						Filters: swarmFilters,
					}

					log.Printf("Setting up Swarm event listener with filters: %v", swarmFilters)
					err := swarmClient.AddEventListenerWithOptions(options, eventChan)
					if err != nil && err != docker.ErrListenerAlreadyExists {
						log.Printf("Error registering docker swarm event listener: %s", err)
						time.Sleep(10 * time.Second)
						continue
					}
					watching = true
					log.Println("Watching docker swarm events")
					// sync all configs after resuming listener
					g.generateFromContainers()
					
					// Start a periodic refresh timer to catch any missed events
					// This helps ensure we catch changes on remote nodes even if events are missed
					go func() {
						// Get configurable refresh interval from the first config that has it set
						refreshInterval := 30 * time.Second // Default value
						for _, cfg := range g.Configs.Config {
							if cfg.SwarmRefreshInterval > 0 {
								refreshInterval = cfg.SwarmRefreshInterval
								break
							}
						}
						
						log.Printf("Starting Swarm periodic refresh with interval: %v", refreshInterval)
						ticker := time.NewTicker(refreshInterval)
						defer ticker.Stop()
						
						for {
							select {
							case <-ticker.C:
								log.Println("Performing periodic Swarm container refresh")
								g.generateFromContainers()
							case <-time.After(60 * time.Second):
								// Exit if the main loop is gone
								return
							}
						}
					}()
				}
				select {
				case event, ok := <-eventChan:
					if !ok {
						log.Printf("Docker swarm connection interrupted")
						if watching {
							swarmClient.RemoveEventListener(eventChan)
							watching = false
							swarmClient = nil
							g.SwarmClient = nil
						}
						if !g.retry {
							// exit but don't close watchers as they're shared with the docker events watcher
							return
						}
						// recreate channel and attempt to resume
						eventChan = make(chan *docker.APIEvents, 100)
						time.Sleep(10 * time.Second)
						break
					}

					// Extract node information if available
					nodeID := "unknown"
					containerID := "n/a"
					serviceID := "n/a"
					
					if nodeAttr, exists := event.Actor.Attributes["com.docker.swarm.node.id"]; exists {
						nodeID = nodeAttr
					}
					
					// Extract container ID if this is a container event
					if event.Type == "container" {
						containerID = event.Actor.ID
					}
					
					// Extract service ID if available
					if serviceAttr, exists := event.Actor.Attributes["com.docker.swarm.service.id"]; exists {
						serviceID = serviceAttr
					}
					
					log.Printf("Received swarm event %s for %s %s (Node: %s, Service: %s)",
						event.Action, event.Type, getIDPrefix(event.Actor.ID), nodeID, serviceID)
					
					// Log additional attributes for debugging
					if len(event.Actor.Attributes) > 0 {
						log.Printf("Swarm event attributes: %v", event.Actor.Attributes)
					}
					
					// If this is a container event on a remote node, show extra information
					if event.Type == "container" && nodeID != "unknown" && nodeID != "" {
						log.Printf("Container event from remote node %s: %s action on container %s",
							nodeID, event.Action, getIDPrefix(containerID))
					}
					
					// fanout event to all watchers
					for _, watcher := range watchers {
						watcher <- event
					}
				case <-time.After(10 * time.Second):
					// check for docker swarm liveness
					err := swarmClient.Ping()
					if err != nil {
						log.Printf("Unable to ping docker swarm: %s", err)
						if watching {
							swarmClient.RemoveEventListener(eventChan)
							watching = false
							swarmClient = nil
							g.SwarmClient = nil
						}
					}
				case sig := <-sigChan:
					log.Printf("Received signal in swarm watcher: %s\n", sig)
					switch sig {
					case syscall.SIGTERM, syscall.SIGINT:
						// exit but don't close watchers as they're shared with the docker events watcher
						return
					}
				}
			}
		}
	}()
}

func (g *generator) runNotifyCmd(config config.Config) {
	if config.NotifyCmd == "" {
		return
	}

	log.Printf("Running '%s'", config.NotifyCmd)
	cmd := exec.Command("/bin/sh", "-c", config.NotifyCmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Error running notify command: %s, %s\n", config.NotifyCmd, err)
	}
	if config.NotifyOutput {
		for _, line := range strings.Split(string(out), "\n") {
			if line != "" {
				log.Printf("[%s]: %s", config.NotifyCmd, line)
			}
		}
	}
}

func (g *generator) sendSignalToContainer(container string, signal int) {
	// Use the appropriate client (either the default Docker client or Swarm client)
	client := g.Client
	clientType := "standard Docker"
	
	// Use the Swarm client if we're in Swarm mode
	if g.SwarmEndpoint != "" && g.SwarmClient != nil {
		client = g.SwarmClient
		clientType = "Docker Swarm"
	}
	
	log.Printf("Sending container '%s' signal '%v' using %s client", container, signal, clientType)

	if signal == -1 {
		log.Printf("Restarting container '%s' using %s client", container, clientType)
		if err := client.RestartContainer(container, 10); err != nil {
			log.Printf("Error restarting container '%s': %s", container, err)
		} else {
			log.Printf("Successfully restarted container '%s'", container)
		}
		return
	}

	killOpts := docker.KillContainerOptions{
		ID:     container,
		Signal: docker.Signal(signal),
	}
	log.Printf("Sending signal %v to container '%s' using %s client", docker.Signal(signal), container, clientType)
	if err := client.KillContainer(killOpts); err != nil {
		log.Printf("Error sending signal %v to container '%s': %s", docker.Signal(signal), container, err)
	} else {
		log.Printf("Successfully sent signal %v to container '%s'", docker.Signal(signal), container)
	}
}

func (g *generator) sendSignalToContainers(config config.Config) {
	if len(config.NotifyContainers) < 1 {
		return
	}

	for container, signal := range config.NotifyContainers {
		g.sendSignalToContainer(container, signal)
	}
}

func (g *generator) sendSignalToFilteredContainers(config config.Config) {
	if len(config.NotifyContainersFilter) < 1 {
		return
	}

	// Determine which client to use - either the default Docker client or Swarm client
	client := g.Client
	clientType := "standard Docker"
	
	// Use the Swarm client if we're in Swarm mode
	if g.SwarmEndpoint != "" && g.SwarmClient != nil {
		client = g.SwarmClient
		clientType = "Docker Swarm"
	}
	
	log.Printf("Filtering containers with filters %v using %s client", config.NotifyContainersFilter, clientType)

	containers, err := client.ListContainers(docker.ListContainersOptions{
		Filters: config.NotifyContainersFilter,
		All:     g.All,
	})
	if err != nil {
		log.Printf("Error getting containers with filters %v: %s", config.NotifyContainersFilter, err)
		return
	}

	log.Printf("Found %d container(s) matching filters %v", len(containers), config.NotifyContainersFilter)
	
	for i, container := range containers {
		log.Printf("Sending signal to filtered container %d/%d: ID=%s Name=%s", 
			i+1, len(containers), getIDPrefix(container.ID), strings.Join(container.Names, ", "))
		g.sendSignalToContainer(container.ID, config.NotifyContainersSignal)
	}
}

func (g *generator) GetSwarmContainers() ([]*context.RuntimeContainer, error) {
	log.Printf("Getting containers from Swarm endpoint: %s", g.SwarmEndpoint)

	if g.SwarmEndpoint == "" {
		return nil, fmt.Errorf("swarm endpoint is not set, cannot get swarm containers")
	}

	// Use the existing SwarmClient
	if g.SwarmClient == nil {
		log.Printf("Swarm client is not initialized")
		return nil, fmt.Errorf("swarm client is not initialized")
	}
	
	log.Printf("Retrieving Docker Swarm server info")
	
	// Get API information for server details
	apiInfo, err := g.SwarmClient.Info()
	if err != nil {
		log.Printf("Error retrieving docker swarm server info: %s\n", err)
	} else {
		context.SetServerInfo(apiInfo)
		log.Printf("Connected to Docker Swarm server: %s (version: %s)", apiInfo.Name, apiInfo.ServerVersion)
	}
	
	log.Printf("Listing services from Swarm")
	services, err := g.SwarmClient.ListServices(docker.ListServicesOptions{})
	if err != nil {
		log.Printf("Error listing services: %s", err)
		return nil, err
	}
	log.Printf("Found %d services in Swarm", len(services))
	
	log.Printf("Listing networks from Swarm")
	apiNetworks, err := g.SwarmClient.ListNetworks()
	if err != nil {
		log.Printf("Error listing networks: %s", err)
		return nil, err
	}
	log.Printf("Found %d networks in Swarm", len(apiNetworks))
	
	networks := make(map[string]docker.Network)
	for _, apiNetwork := range apiNetworks {
		networks[apiNetwork.Name] = apiNetwork
	}
	
	var containers []*context.RuntimeContainer
	for i, service := range services {
		serviceIDPrefix := getIDPrefix(service.ID)
		log.Printf("Processing service %d/%d: %s (ID: %s)", i+1, len(services), service.Spec.Name, serviceIDPrefix)
		
		tasks, err := g.SwarmClient.ListTasks(docker.ListTasksOptions{
			Filters: map[string][]string{
				"service": {service.ID},
				"desired-state": {"running"},
			},
		})
		if err != nil {
			log.Printf("Error listing tasks for service %s: %s", service.Spec.Name, err)
			continue
		}
		log.Printf("Found %d running tasks for service %s", len(tasks), service.Spec.Name)
		for taskIndex, task := range tasks {
			log.Printf("Processing task %d/%d for service %s", taskIndex+1, len(tasks), service.Spec.Name)
			
			if task.Status.ContainerStatus == nil {
				log.Printf("Task %s has no container status, skipping", getIDPrefix(task.ID))
				continue
			}
			
			if task.Status.ContainerStatus.ContainerID == "" {
				log.Printf("Task %s has empty container ID, skipping", getIDPrefix(task.ID))
				continue
			}
			
			containerID := task.Status.ContainerStatus.ContainerID
			
			// Safely extract the node ID and attempt to get node name
			nodeID := ""
			nodeName := ""
			
			if task.NodeID != "" {
				nodeID = task.NodeID
				
				// Attempt to get more information about the node
				node, err := g.SwarmClient.InspectNode(nodeID)
				if err != nil {
					log.Printf("Error inspecting node %s: %s - will use ID as name", nodeID, err)
					nodeName = nodeID // Fallback to ID if we can't get the name
				} else {
					// Extract the node's hostname (usually more user-friendly than ID)
					if node.Description.Hostname != "" {
						nodeName = node.Description.Hostname
					} else if node.Spec.Name != "" {
						nodeName = node.Spec.Name
					} else {
						nodeName = nodeID // Fallback to ID
					}
					log.Printf("Resolved node %s to hostname %s", nodeID, nodeName)
				}
			}
			
			log.Printf("Inspecting container %s for task %s on node %s", getIDPrefix(containerID), getIDPrefix(task.ID), nodeName)
			
			container, err := g.SwarmClient.InspectContainer(containerID)
			if err != nil {
				log.Printf("Warning: Cannot inspect container %s on remote node %s: %s - will use task information", getIDPrefix(containerID), nodeName, err)
				// Container is on a different node, we'll create a RuntimeContainer from task info
				container = nil
			} else {
				log.Printf("Successfully inspected container: %s (Name: %s)", getIDPrefix(containerID), container.Name)
			}
			// Using a more defensive approach for accessing container fields
	var registry, repository, tag string
	// containerID is already defined above
	var created time.Time
	var running bool
	var name string
	var hostname string
	var gateway string
	var networkMode string
	var addresses []context.Address
	var labels = make(map[string]string) // Initialize to empty map to avoid nil
	var ipAddress string
	var ip6LinkLocal string
	var ip6Global string
	
	// Safely extract all values using defer/recover pattern
	func() {
		defer func() {
			if r := recover(); r != nil {
				// Just recover and continue with default values
			}
		}()
		
		if container != nil {
			// Extract from container inspection (local node)
			containerID = container.ID
			created = container.Created
			
			if container.Config != nil {
				registry, repository, tag = dockerclient.SplitDockerImage(container.Config.Image)
				hostname = container.Config.Hostname
				if container.Config.Labels != nil {
					labels = container.Config.Labels
				}
			}
			
			if container.State.Running {
				running = true
			}
			
			if container.Name != "" {
				name = strings.TrimLeft(container.Name, "/")
			}
			
			if container.NetworkSettings != nil {
				gateway = container.NetworkSettings.Gateway
				ipAddress = container.NetworkSettings.IPAddress
				ip6LinkLocal = container.NetworkSettings.LinkLocalIPv6Address
				ip6Global = container.NetworkSettings.GlobalIPv6Address
			}
			
			if container.HostConfig != nil {
				networkMode = container.HostConfig.NetworkMode
			}
			
			addresses = context.GetContainerAddresses(container)
		} else {
			// Extract from Swarm task/service information (remote node)
			// We have limited information but can still create a useful container object
			containerID = getIDPrefix(task.Status.ContainerStatus.ContainerID)
			
			// Task created time
			created = task.CreatedAt
			
			// Extract image information from task spec
			if task.Spec.ContainerSpec != nil && task.Spec.ContainerSpec.Image != "" {
				registry, repository, tag = dockerclient.SplitDockerImage(task.Spec.ContainerSpec.Image)
				
				// Extract hostname from container spec
				if task.Spec.ContainerSpec.Hostname != "" {
					hostname = task.Spec.ContainerSpec.Hostname
				}
				
				// Extract labels from container spec
				if task.Spec.ContainerSpec.Labels != nil {
					labels = make(map[string]string)
					for k, v := range task.Spec.ContainerSpec.Labels {
						labels[k] = v
					}
				}
			}
			
			// Check if task is running
			if task.Status.State == "running" {
				running = true
			}
			
			// Generate a name from service name and task index/ID
			if service.ID != "" {
				name = fmt.Sprintf("%s.%s", service.Spec.Name, getIDPrefix(task.ID))
			} else {
				name = getIDPrefix(task.ID)
			}
			
			log.Printf("Created container info from task data for remote container %s on node %s", getIDPrefix(containerID), nodeName)
		}
	}()
	
	// Create the runtime container
	runtimeContainer := &context.RuntimeContainer{
		ID:      containerID,
		Created: created,
		Image: context.DockerImage{
			Registry:   registry,
			Repository: repository,
			Tag:        tag,
		},
		State: context.State{
			Running: running,
			Health: context.Health{
				Status: getContainerHealthStatus(container),
			},
		},
		Name:         name,
		Hostname:     hostname,
		Gateway:      gateway,
		NetworkMode:  networkMode,
		Addresses:    addresses,
		Networks:     []context.Network{},
		Env:          make(map[string]string),
		Volumes:      make(map[string]context.Volume),
		Node: context.SwarmNode{
			ID:   nodeID,
			Name: nodeName, // We now have the actual node name from our lookup
			Address: context.Address{
				// We could add more node address information here if needed
				// For now, it's sufficient to have the ID and name
			},
		},
		Labels:       labels, // This is already safely extracted in the deferred function
		IP:           ipAddress,
		IP6LinkLocal: ip6LinkLocal,
		IP6Global:    ip6Global,
	}
			// Safely iterate over networks with nil check
			if container != nil && container.NetworkSettings != nil && container.NetworkSettings.Networks != nil {
				for k, v := range container.NetworkSettings.Networks {
					// Make sure the network exists in our networks map
					networkInternal := false
					if net, exists := networks[k]; exists {
						networkInternal = net.Internal
					}
					
					network := context.Network{
						IP:                  v.IPAddress,
						Name:                k,
						Gateway:             v.Gateway,
						EndpointID:          v.EndpointID,
						IPv6Gateway:         v.IPv6Gateway,
						GlobalIPv6Address:   v.GlobalIPv6Address,
						MacAddress:          v.MacAddress,
						GlobalIPv6PrefixLen: v.GlobalIPv6PrefixLen,
						IPPrefixLen:         v.IPPrefixLen,
						Internal:            networkInternal,
					}
					runtimeContainer.Networks = append(runtimeContainer.Networks, network)
				}
			}
			// Safely iterate over volumes with nil checks
			if container != nil && container.Volumes != nil && container.VolumesRW != nil {
				for k, v := range container.Volumes {
					// Check if the key exists in VolumesRW map
					readWrite := false
					if rw, exists := container.VolumesRW[k]; exists {
						readWrite = rw
					}
					
					runtimeContainer.Volumes[k] = context.Volume{
						Path:      k,
						HostPath:  v,
						ReadWrite: readWrite,
					}
				}
			}
			// Safely iterate over mounts with nil check
			if container != nil && container.Mounts != nil {
				for _, v := range container.Mounts {
					runtimeContainer.Mounts = append(runtimeContainer.Mounts, context.Mount{
						Name:        v.Name,
						Source:      v.Source,
						Destination: v.Destination,
						Driver:      v.Driver,
						Mode:        v.Mode,
						RW:          v.RW,
					})
				}
			}
			
			// Safely set environment variables
			if container != nil && container.Config != nil && container.Config.Env != nil {
				runtimeContainer.Env = utils.SplitKeyValueSlice(container.Config.Env)
			}
			containers = append(containers, runtimeContainer)
		}
	}
	return containers, nil
}

func (g *generator) getContainers() ([]*context.RuntimeContainer, error) {
	// If Swarm endpoint is set, use the Swarm API
	if g.SwarmEndpoint != "" {
		return g.GetSwarmContainers()
	}

	// Otherwise use the standard Docker API
	apiInfo, err := g.Client.Info()
	if err != nil {
		log.Printf("Error retrieving docker server info: %s\n", err)
	} else {
		context.SetServerInfo(apiInfo)
	}

	apiContainers, err := g.Client.ListContainers(docker.ListContainersOptions{
		All:  g.All,
		Size: false,
	})
	if err != nil {
		return nil, err
	}

	apiNetworks, err := g.Client.ListNetworks()
	if err != nil {
		return nil, err
	}
	networks := make(map[string]docker.Network)
	for _, apiNetwork := range apiNetworks {
		networks[apiNetwork.Name] = apiNetwork
	}

	containers := []*context.RuntimeContainer{}
	for _, apiContainer := range apiContainers {
		opts := docker.InspectContainerOptions{ID: apiContainer.ID}
		container, err := g.Client.InspectContainerWithOptions(opts)
		if err != nil {
			log.Printf("Error inspecting container: %s: %s\n", apiContainer.ID, err)
			continue
		}

		// Using a more defensive approach for accessing container fields
		var registry, repository, tag string
		var containerID string
		var created time.Time
		var running bool
		var name string
		var hostname string
		var gateway string
		var networkMode string
		var ipAddress string
		var ip6LinkLocal string
		var ip6Global string
		
		// Safely extract all values using defer/recover pattern
		func() {
			defer func() {
				if r := recover(); r != nil {
					// Just recover and continue with default values
				}
			}()
			
			if container != nil {
				containerID = container.ID
				created = container.Created
				
				if container.Config != nil {
					registry, repository, tag = dockerclient.SplitDockerImage(container.Config.Image)
					hostname = container.Config.Hostname
				}
				
				if container.State.Running {
					running = true
				}
				
				if container.Name != "" {
					name = strings.TrimLeft(container.Name, "/")
				}
				
				if container.NetworkSettings != nil {
					gateway = container.NetworkSettings.Gateway
					ipAddress = container.NetworkSettings.IPAddress
					ip6LinkLocal = container.NetworkSettings.LinkLocalIPv6Address
					ip6Global = container.NetworkSettings.GlobalIPv6Address
				}
				
				if container.HostConfig != nil {
					networkMode = container.HostConfig.NetworkMode
				}
			}
		}()
		
		runtimeContainer := &context.RuntimeContainer{
			ID:      containerID,
			Created: created,
			Image: context.DockerImage{
				Registry:   registry,
				Repository: repository,
				Tag:        tag,
			},
			State: context.State{
				Running: running,
				Health: context.Health{
					Status: getContainerHealthStatus(container),
				},
			},
			Name:         name,
			Hostname:     hostname,
			Gateway:      gateway,
			NetworkMode:  networkMode,
			Addresses:    []context.Address{},
			Networks:     []context.Network{},
			Env:          make(map[string]string),
			Volumes:      make(map[string]context.Volume),
			Node:         context.SwarmNode{},
			Labels:       make(map[string]string),
			IP:           ipAddress,
			IP6LinkLocal: ip6LinkLocal,
			IP6Global:    ip6Global,
		}

		adresses := context.GetContainerAddresses(container)
		runtimeContainer.Addresses = append(runtimeContainer.Addresses, adresses...)

		for k, v := range container.NetworkSettings.Networks {
			network := context.Network{
				IP:                  v.IPAddress,
				Name:                k,
				Gateway:             v.Gateway,
				EndpointID:          v.EndpointID,
				IPv6Gateway:         v.IPv6Gateway,
				GlobalIPv6Address:   v.GlobalIPv6Address,
				MacAddress:          v.MacAddress,
				GlobalIPv6PrefixLen: v.GlobalIPv6PrefixLen,
				IPPrefixLen:         v.IPPrefixLen,
				Internal:            networks[k].Internal,
			}

			runtimeContainer.Networks = append(runtimeContainer.Networks,
				network)
		}

		for k, v := range container.Volumes {
			runtimeContainer.Volumes[k] = context.Volume{
				Path:      k,
				HostPath:  v,
				ReadWrite: container.VolumesRW[k],
			}
		}
		if container.Node != nil {
			runtimeContainer.Node.ID = container.Node.ID
			runtimeContainer.Node.Name = container.Node.Name
			runtimeContainer.Node.Address = context.Address{
				IP: container.Node.IP,
			}
		}

		for _, v := range container.Mounts {
			runtimeContainer.Mounts = append(runtimeContainer.Mounts, context.Mount{
				Name:        v.Name,
				Source:      v.Source,
				Destination: v.Destination,
				Driver:      v.Driver,
				Mode:        v.Mode,
				RW:          v.RW,
			})
		}

		runtimeContainer.Env = utils.SplitKeyValueSlice(container.Config.Env)
		runtimeContainer.Labels = container.Config.Labels
		containers = append(containers, runtimeContainer)
	}
	return containers, nil

}

func newSignalChannel() (<-chan os.Signal, func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	return sig, func() { signal.Stop(sig) }
}

func newDebounceChannel(input chan *docker.APIEvents, wait *config.Wait) chan *docker.APIEvents {
	if wait == nil {
		return input
	}
	if wait.Min == 0 {
		return input
	}

	output := make(chan *docker.APIEvents, 100)

	go func() {
		var (
			event    *docker.APIEvents
			minTimer <-chan time.Time
			maxTimer <-chan time.Time
		)

		defer close(output)

		for {
			select {
			case buffer, ok := <-input:
				if !ok {
					return
				}
				event = buffer
				minTimer = time.After(wait.Min)
				if maxTimer == nil {
					maxTimer = time.After(wait.Max)
				}
			case <-minTimer:
				log.Println("Debounce minTimer fired")
				minTimer, maxTimer = nil, nil
				output <- event
			case <-maxTimer:
				log.Println("Debounce maxTimer fired")
				minTimer, maxTimer = nil, nil
				output <- event
			}
		}
	}()

	return output
}
