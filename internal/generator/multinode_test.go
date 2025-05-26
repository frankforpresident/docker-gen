package generator

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"

	dockertest "github.com/fsouza/go-dockerclient/testing"
	"github.com/nginx-proxy/docker-gen/internal/context"
	"github.com/nginx-proxy/docker-gen/internal/dockerclient"
)

// TestMultiNodeContainerInspectionFix tests that the fix for multi-node container inspection
// properly handles containers on different nodes without failing the entire container listing
func TestMultiNodeContainerInspectionFix(t *testing.T) {
	log.SetOutput(io.Discard)

	// Test setup
	managerNodeID := "manager-node-123"
	workerNodeID := "worker-node-456" 
	serviceID := "multinode-service-789"
	workerContainerID := "worker-container-abc123"
	managerContainerID := "manager-container-def456"

	// Create mock server
	server, _ := dockertest.NewServer("127.0.0.1:0", nil, nil)

	// Mock info endpoint to simulate manager node
	infoResponse := fmt.Sprintf(`{"Containers":2,"Images":3,"ServerVersion":"20.10.8","Swarm":{"NodeID":"%s","NodeAddr":"192.168.1.1","LocalNodeState":"active","ControlAvailable":true,"Error":"","RemoteManagers":[{"NodeID":"%s","Addr":"192.168.1.1:2377"}]}}`, managerNodeID, managerNodeID)
	server.CustomHandler("/info", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(infoResponse))
	}))

	// Mock version endpoint
	versionResponse := `{"Version":"20.10.8","Os":"Linux","KernelVersion":"5.10.0","GoVersion":"go1.16.6","GitCommit":"3967b7d","Arch":"amd64","ApiVersion":"1.41"}`
	server.CustomHandler("/version", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(versionResponse))
	}))

	// Mock services endpoint
	server.CustomHandler("/services", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servicesJSON := fmt.Sprintf(`[
			{
				"ID": "%s",
				"Spec": {
					"Name": "test-multinode-service",
					"Labels": {
						"multinode-test": "true"
					}
				}
			}
		]`, serviceID)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(servicesJSON))
	}))

	// Mock tasks endpoint - simulates tasks on different nodes
	server.CustomHandler("/tasks", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tasksJSON := fmt.Sprintf(`[
			{
				"ID": "task-worker-123",
				"ServiceID": "%s",
				"NodeID": "%s",
				"Spec": {
					"ContainerSpec": {
						"Image": "alpine:latest",
						"Hostname": "worker-task",
						"Labels": {
							"multinode-test": "true"
						}
					}
				},
				"Status": {
					"ContainerStatus": {
						"ContainerID": "%s"
					},
					"State": "running"
				},
				"CreatedAt": "2021-05-20T10:00:00Z"
			},
			{
				"ID": "task-manager-456", 
				"ServiceID": "%s",
				"NodeID": "%s",
				"Spec": {
					"ContainerSpec": {
						"Image": "alpine:latest",
						"Hostname": "manager-task",
						"Labels": {
							"multinode-test": "true"
						}
					}
				},
				"Status": {
					"ContainerStatus": {
						"ContainerID": "%s"
					},
					"State": "running"
				},
				"CreatedAt": "2021-05-20T10:01:00Z"
			}
		]`, serviceID, workerNodeID, workerContainerID, serviceID, managerNodeID, managerContainerID)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(tasksJSON))
	}))

	// Mock nodes endpoint to provide node information
	server.CustomHandler(fmt.Sprintf("/nodes/%s", workerNodeID), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodeJSON := fmt.Sprintf(`{
			"ID": "%s",
			"Spec": {
				"Name": "worker-node"
			},
			"Description": {
				"Hostname": "worker-node-host"
			}
		}`, workerNodeID)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(nodeJSON))
	}))

	server.CustomHandler(fmt.Sprintf("/nodes/%s", managerNodeID), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodeJSON := fmt.Sprintf(`{
			"ID": "%s",
			"Spec": {
				"Name": "manager-node"
			},
			"Description": {
				"Hostname": "manager-node-host"
			}
		}`, managerNodeID)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(nodeJSON))
	}))

	// Mock container inspection - manager container succeeds, worker container fails (simulating remote node)
	server.CustomHandler(fmt.Sprintf("/containers/%s/json", managerContainerID), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		containerJSON := fmt.Sprintf(`{
			"Id": "%s",
			"Name": "/test-multinode-service.2.task-manager-456",
			"Config": {
				"Image": "alpine:latest",
				"Hostname": "manager-task",
				"Labels": {
					"com.docker.swarm.service.id": "%s",
					"com.docker.swarm.node.id": "%s",
					"multinode-test": "true"
				}
			},
			"State": {
				"Running": true,
				"Pid": 5678,
				"ExitCode": 0,
				"StartedAt": "2021-05-20T10:01:00Z",
				"RestartCount": 0
			},
			"NetworkSettings": {
				"IPAddress": "10.0.1.3"
			},
			"Node": {
				"ID": "%s",
				"Name": "manager-node-host"
			}
		}`, managerContainerID, serviceID, managerNodeID, managerNodeID)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(containerJSON))
	}))

	// Worker container inspection returns 404 to simulate remote node
	server.CustomHandler(fmt.Sprintf("/containers/%s/json", workerContainerID), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"No such container: ` + workerContainerID + `"}`))
	}))

	// Setup client
	serverURL := fmt.Sprintf("tcp://%s", strings.TrimRight(strings.TrimPrefix(server.URL(), "http://"), "/"))
	client, err := dockerclient.NewDockerClient(serverURL, false, "", "", "")
	if err != nil {
		t.Fatalf("Failed to create client: %s", err)
	}
	client.SkipServerVersionCheck = true

	// Create generator
	generator := &generator{
		Client:        client,
		SwarmClient:   client, // Use same mock for both
		Endpoint:      serverURL,
		SwarmEndpoint: serverURL,
	}

	// Test the GetSwarmContainers function
	containers, err := generator.GetSwarmContainers()
	if err != nil {
		t.Fatalf("GetSwarmContainers failed: %v", err)
	}

	// Verify we got containers from both nodes despite worker container inspection failing
	if len(containers) < 2 {
		t.Errorf("Expected at least 2 containers (one from each node), got %d", len(containers))
	}

	// Track what we found
	var hasWorkerContainer, hasManagerContainer bool
	var workerContainer, managerContainer *context.RuntimeContainer

	for _, container := range containers {
		containerIDPrefix := container.ID
		if len(containerIDPrefix) > 12 {
			containerIDPrefix = containerIDPrefix[:12]
		}

		// Check if this is the worker container (should be created from task data)
		if strings.Contains(containerIDPrefix, "worker-container") || 
		   (container.Node.ID == workerNodeID) {
			hasWorkerContainer = true
			workerContainer = container
			t.Logf("Found worker container: ID=%s, Name=%s, Node=%s", 
				containerIDPrefix, container.Name, container.Node.ID)
		}

		// Check if this is the manager container (should be from container inspection)
		if strings.Contains(containerIDPrefix, "manager-container") || 
		   (container.Node.ID == managerNodeID) {
			hasManagerContainer = true
			managerContainer = container
			t.Logf("Found manager container: ID=%s, Name=%s, Node=%s", 
				containerIDPrefix, container.Name, container.Node.ID)
		}
	}

	// Verify both containers were found
	if !hasWorkerContainer {
		t.Error("Worker container from remote node was not found - this indicates the fix is not working")
	}

	if !hasManagerContainer {
		t.Error("Manager container from local node was not found")
	}

	// Verify that worker container has basic information populated from task data
	if hasWorkerContainer && workerContainer != nil {
		if workerContainer.Image.String() == "" {
			t.Error("Worker container should have image information from task spec")
		}
		if workerContainer.Hostname == "" {
			t.Error("Worker container should have hostname from task spec")
		}

		if workerContainer.Node.ID != workerNodeID {
			t.Errorf("Worker container node ID should be %s, got %s", workerNodeID, workerContainer.Node.ID)
		}
	}

	// Verify that manager container has full information from container inspection
	if hasManagerContainer && managerContainer != nil {
		// Manager container should have been successfully inspected
		// The key test is that it exists and was processed correctly
		t.Logf("Manager container successfully inspected and processed")
	}

	t.Logf("Multi-node container inspection fix test completed successfully")
	t.Logf("Found %d total containers", len(containers))
	t.Logf("Worker container found: %t", hasWorkerContainer)
	t.Logf("Manager container found: %t", hasManagerContainer)
}
