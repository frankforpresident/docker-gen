package main

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/nginx-proxy/docker-gen/internal/dockerclient"
)

func main() {
	// Connect to Docker Swarm
	endpoint := "unix:///var/run/docker.sock"
	
	client, err := dockerclient.NewDockerClient(endpoint, false, "", "", "")
	if err != nil {
		log.Fatalf("Error creating Docker client: %v", err)
	}

	// Check if we're in Swarm mode
	info, err := client.Info()
	if err != nil {
		log.Fatalf("Error getting Docker info: %v", err)
	}

	if info.Swarm.LocalNodeState != "active" {
		log.Fatalf("Not in an active Swarm cluster. Local node state: %s", info.Swarm.LocalNodeState)
	}

	log.Printf("Connected to Swarm cluster:")
	log.Printf("  Node ID: %s", info.Swarm.NodeID)
	log.Printf("  Node Address: %s", info.Swarm.NodeAddr)
	log.Printf("  Is Manager: %t", info.Swarm.ControlAvailable)
	log.Printf("  Remote Managers: %d", len(info.Swarm.RemoteManagers))

	// List all nodes in the cluster
	nodes, err := client.ListNodes(docker.ListNodesOptions{})
	if err != nil {
		log.Printf("Error listing nodes: %v", err)
	} else {
		log.Printf("\nSwarm Cluster Nodes (%d total):", len(nodes))
		for i, node := range nodes {
			role := "worker"
			if node.Spec.Role == "manager" {
				role = "manager"
			}
			
			hostname := "unknown"
			if node.Description.Hostname != "" {
				hostname = node.Description.Hostname
			}
			
			status := string(node.Status.State)
			availability := string(node.Spec.Availability)
			
			nodeID := node.ID
			if len(nodeID) >= 12 {
				nodeID = nodeID[:12]
			}
			
			log.Printf("  %d. Node ID: %s", i+1, nodeID)
			log.Printf("     Hostname: %s", hostname)
			log.Printf("     Role: %s", role)
			log.Printf("     Status: %s", status)
			log.Printf("     Availability: %s", availability)
			log.Printf("     Address: %s", node.Status.Addr)
			
			if node.ID == info.Swarm.NodeID {
				log.Printf("     ^ This is the current node")
			}
			log.Println()
		}
	}

	// List all services
	services, err := client.ListServices(docker.ListServicesOptions{})
	if err != nil {
		log.Printf("Error listing services: %v", err)
	} else {
		log.Printf("Swarm Services (%d total):", len(services))
		for i, service := range services {
			serviceID := service.ID
			if len(serviceID) >= 12 {
				serviceID = serviceID[:12]
			}
			log.Printf("  %d. Service: %s (ID: %s)", i+1, service.Spec.Name, serviceID)
			
			// List tasks for this service
			tasks, err := client.ListTasks(docker.ListTasksOptions{
				Filters: map[string][]string{
					"service": {service.ID},
				},
			})
			if err != nil {
				log.Printf("     Error listing tasks: %v", err)
			} else {
				log.Printf("     Tasks (%d):", len(tasks))
				for j, task := range tasks {
					containerID := "n/a"
					if task.Status.ContainerStatus != nil && task.Status.ContainerStatus.ContainerID != "" {
						if len(task.Status.ContainerStatus.ContainerID) >= 12 {
							containerID = task.Status.ContainerStatus.ContainerID[:12]
						} else {
							containerID = task.Status.ContainerStatus.ContainerID
						}
					}
					
					taskID := task.ID
					if len(taskID) >= 12 {
						taskID = taskID[:12]
					}
					
					nodeID := task.NodeID
					if len(nodeID) >= 12 {
						nodeID = nodeID[:12]
					}
					
					log.Printf("       %d. Task ID: %s", j+1, taskID)
					log.Printf("          Node ID: %s", nodeID)
					log.Printf("          Container ID: %s", containerID)
					log.Printf("          State: %s", task.Status.State)
					log.Printf("          Desired State: %s", task.DesiredState)
				}
			}
			log.Println()
		}
	}

	// Set up event monitoring with enhanced filters for debugging
	log.Println("Setting up event monitoring for debugging remote node events...")
	
	// Test different event filter combinations
	testEventFilters := []map[string][]string{
		// Basic Swarm events
		{
			"type":  {"container", "service", "node"},
			"swarm": {"true"},
		},
		// More inclusive filters
		{
			"type": {"container", "service", "node", "task"},
		},
		// Container-only events with Swarm filter
		{
			"type":  {"container"},
			"swarm": {"true"},
		},
	}

	for i, filters := range testEventFilters {
		log.Printf("\n=== Testing Event Filter Set %d ===", i+1)
		log.Printf("Filters: %+v", filters)
		
		go func(filterSet int, eventFilters map[string][]string) {
			eventChan := make(chan *docker.APIEvents, 100)
			
			options := docker.EventsOptions{
				Filters: eventFilters,
			}
			
			err := client.AddEventListenerWithOptions(options, eventChan)
			if err != nil {
				log.Printf("Filter Set %d: Error adding event listener: %v", filterSet, err)
				return
			}
			
			log.Printf("Filter Set %d: Event listener started", filterSet)
			
			// Listen for events for 60 seconds
			timeout := time.After(60 * time.Second)
			
			for {
				select {
				case event := <-eventChan:
					nodeID := "unknown"
					serviceID := "n/a"
					containerID := "n/a"
					
					if event.Actor.Attributes != nil {
						if id, exists := event.Actor.Attributes["com.docker.swarm.node.id"]; exists {
							if len(id) >= 12 {
								nodeID = id[:12]
							} else {
								nodeID = id
							}
						}
						if id, exists := event.Actor.Attributes["com.docker.swarm.service.id"]; exists {
							if len(id) >= 12 {
								serviceID = id[:12]
							} else {
								serviceID = id
							}
						}
					}
					
					actorID := event.Actor.ID
					if len(actorID) >= 12 {
						actorID = actorID[:12]
					}
					
					if event.Type == "container" {
						containerID = actorID
					}
					
					log.Printf("Filter Set %d: EVENT - Type: %s, Action: %s, Actor: %s, Node: %s, Service: %s, Container: %s, Time: %d", 
						filterSet, event.Type, event.Action, actorID, nodeID, serviceID, containerID, event.Time)
					
					if len(event.Actor.Attributes) > 0 {
						attrJson, _ := json.MarshalIndent(event.Actor.Attributes, "    ", "  ")
						log.Printf("Filter Set %d: Attributes: %s", filterSet, string(attrJson))
					}
					
				case <-timeout:
					log.Printf("Filter Set %d: Timeout reached, stopping event monitoring", filterSet)
					client.RemoveEventListener(eventChan)
					return
				}
			}
		}(i+1, filters)
	}

	// Instructions for testing
	log.Println("\n=== TESTING INSTRUCTIONS ===")
	log.Println("This tool will monitor Swarm events for 60 seconds with different filter sets.")
	log.Println("To test remote node event detection:")
	log.Println("1. On a DIFFERENT node in the cluster (not this manager node):")
	log.Println("   - Start a container: docker run -d --name test-remote nginx")
	log.Println("   - Stop the container: docker stop test-remote")
	log.Println("   - Remove the container: docker rm test-remote")
	log.Println("2. Or scale a service up/down:")
	log.Println("   - docker service create --name test-service --replicas 1 nginx")
	log.Println("   - docker service scale test-service=0")
	log.Println("   - docker service rm test-service")
	log.Println("3. Watch the output above to see which events are captured")
	log.Println("===========================================")

	// Keep the program running to monitor events
	scanner := bufio.NewScanner(os.Stdin)
	log.Println("\nPress Enter to exit...")
	scanner.Scan()
}
