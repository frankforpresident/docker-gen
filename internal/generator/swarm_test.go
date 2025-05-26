package generator

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	dockertest "github.com/fsouza/go-dockerclient/testing"
	"github.com/nginx-proxy/docker-gen/internal/config"
	"github.com/nginx-proxy/docker-gen/internal/context"
	"github.com/nginx-proxy/docker-gen/internal/dockerclient"
	"github.com/nginx-proxy/docker-gen/internal/template"
)

func TestSwarmContainerNotifications(t *testing.T) {
	log.SetOutput(io.Discard)
	
	// Test container IDs
	swarmContainerID := "c9f97ac9cd87"
	serviceID := "srv123456789"
	taskID := "task123456789"
	var counter atomic.Int32
	var signalSent atomic.Bool

	// Mock a simple Swarm service event
	eventsResponse := `
{"Type":"service","Action":"update","Actor":{"ID":"srv123456789","Attributes":{"name":"test-service"}},"scope":"swarm","time":1590000000}
{"Type":"container","Action":"start","Actor":{"ID":"c9f97ac9cd87","Attributes":{"com.docker.swarm.service.id":"srv123456789"}},"scope":"swarm","time":1590000001}`

	// Create mock server with custom handlers
	server, _ := dockertest.NewServer("127.0.0.1:0", nil, nil)
	
	// Mock /info endpoint for Swarm info
	infoResponse := `{"Containers":3,"Images":2,"ServerVersion":"20.10.8","Swarm":{"NodeID":"nodeabc123","NodeAddr":"192.168.1.1","LocalNodeState":"active","ControlAvailable":true,"Error":"","RemoteManagers":[{"NodeID":"nodeabc123","Addr":"192.168.1.1:2377"}]}}`
	server.CustomHandler("/info", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(infoResponse))
		w.(http.Flusher).Flush()
	}))

	// Mock /version endpoint
	versionResponse := `{"Version":"20.10.8","Os":"Linux","KernelVersion":"5.10.0","GoVersion":"go1.16.6","GitCommit":"3967b7d","Arch":"amd64","ApiVersion":"1.41"}`
	server.CustomHandler("/version", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(versionResponse))
		w.(http.Flusher).Flush()
	}))

	// Mock events endpoint
	server.CustomHandler("/events", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rsc := bufio.NewScanner(strings.NewReader(eventsResponse))
		for rsc.Scan() {
			w.Write([]byte(rsc.Text()))
			w.(http.Flusher).Flush()
			time.Sleep(150 * time.Millisecond)
		}
		time.Sleep(500 * time.Millisecond)
	}))

	// Mock /services endpoint to list services using raw JSON instead of structs
	server.CustomHandler("/services", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Create a JSON response with the correct structure but without using undefined types
		servicesJSON := fmt.Sprintf(`
		[
			{
				"ID": "%s",
				"Spec": {
					"Name": "test-service",
					"Labels": {
						"notify": "true"
					}
				}
			}
		]`, serviceID)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(servicesJSON))
	}))

	// Mock /tasks endpoint for listing tasks using raw JSON
	server.CustomHandler("/tasks", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if filter contains the service ID
		if strings.Contains(r.URL.RawQuery, serviceID) {
			// Use raw JSON instead of structs
			tasksJSON := fmt.Sprintf(`
			[
				{
					"ID": "%s",
					"ServiceID": "%s",
					"NodeID": "nodeabc123",
					"Status": {
						"ContainerStatus": {
							"ContainerID": "%s"
						},
						"State": "running"
					}
				}
			]`, taskID, serviceID, swarmContainerID)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(tasksJSON))
		} else {
			// Return empty if not filtered by our service
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("[]"))
		}
	}))

	// Mock /networks endpoint
	server.CustomHandler("/networks", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		networks := []docker.Network{
			{
				Name:     "overlay-network",
				ID:       "net123456789",
				Driver:   "overlay", // Use Driver instead of Type which doesn't exist
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(networks)
	}))

	// Mock container list endpoint for filtered containers
	server.CustomHandler("/containers/json", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if we're filtering by label
		if strings.Contains(r.URL.RawQuery, "notify=true") {
			result := []docker.APIContainers{
				{
					ID:      swarmContainerID,
					Image:   "alpine:latest",
					Command: "/bin/sh",
					Created: time.Now().Unix(),
					Status:  "running",
					Ports:   []docker.APIPort{},
					Names:   []string{"/test-service.1.task123456789"},
					Labels: map[string]string{
						"notify": "true",
						"com.docker.swarm.service.id": serviceID,
						"com.docker.swarm.task.id":    taskID,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(result)
		} else {
			// Return the container anyway for other list operations
			result := []docker.APIContainers{
				{
					ID:      swarmContainerID,
					Image:   "alpine:latest",
					Command: "/bin/sh",
					Created: time.Now().Unix(),
					Status:  "running",
					Ports:   []docker.APIPort{},
					Names:   []string{"/test-service.1.task123456789"},
					Labels: map[string]string{
						"com.docker.swarm.service.id": serviceID,
						"com.docker.swarm.task.id":    taskID,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(result)
		}
	}))

	// Mock specific container inspection
	server.CustomHandler(fmt.Sprintf("/containers/%s/json", swarmContainerID), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter := counter.Add(1)
		container := docker.Container{
			Name:    "test-service.1.task123456789",
			ID:      swarmContainerID,
			Created: time.Now(),
			Path:    "/bin/sh",
			Args:    []string{},
			Config: &docker.Config{
				Hostname:     "task123456789",
				AttachStdout: true,
				AttachStderr: true,
				Env:          []string{fmt.Sprintf("COUNTER=%d", counter)},
				Cmd:          []string{"/bin/sh"},
				Image:        "alpine:latest",
				Labels: map[string]string{
					"com.docker.swarm.service.id": serviceID,
					"com.docker.swarm.task.id":    taskID,
					"notify":                      "true",
				},
			},
			HostConfig: &docker.HostConfig{
				NetworkMode: "overlay-network",
			},
			State: docker.State{
				Running:   true,
				Pid:       400,
				ExitCode:  0,
				StartedAt: time.Now(),
				Health: docker.Health{
					Status:        "healthy",
					FailingStreak: 0,
					Log:           []docker.HealthCheck{},
				},
			},
			Image: "alpine@sha256:def822f9851ca422481ec6d7c86391f2",
			NetworkSettings: &docker.NetworkSettings{
				IPAddress:   "10.0.0.2",
				IPPrefixLen: 24,
				Gateway:     "10.0.0.1",
				Bridge:      "docker0",
				PortMapping: map[string]docker.PortMapping{},
				Ports:       map[docker.Port][]docker.PortBinding{},
				Networks: map[string]docker.ContainerNetwork{
					"overlay-network": {
						IPAddress:   "10.0.0.2",
						IPPrefixLen: 24,
						Gateway:     "10.0.0.1",
						EndpointID:  "ep123456789",
					},
				},
			},
			// Use nil for Node since we can't easily set a docker.SwarmNode
			Node: nil,
			ResolvConfPath: "/etc/resolv.conf",
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(container)
	}))

	// Mock container kill endpoint to test signal sending
	server.CustomHandler(fmt.Sprintf("/containers/%s/kill", swarmContainerID), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check for signal parameter
		signal := r.URL.Query().Get("signal")
		if signal == "1" || signal == "HUP" { // Check for SIGHUP
			signalSent.Store(true)
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))

	// Setup server URL and create Docker client
	serverURL := fmt.Sprintf("tcp://%s", strings.TrimRight(strings.TrimPrefix(server.URL(), "http://"), "/"))
	client, err := dockerclient.NewDockerClient(serverURL, false, "", "", "")
	if err != nil {
		t.Errorf("Failed to create client: %s", err)
	}
	client.SkipServerVersionCheck = true

	// Create temporary template file
	tmplFile, err := os.CreateTemp(os.TempDir(), "docker-gen-tmpl")
	if err != nil {
		t.Errorf("Failed to create temp file: %v\n", err)
	}
	defer func() {
		tmplFile.Close()
		os.Remove(tmplFile.Name())
	}()
	
	// Write a simple template that lists container IDs and counter values
	err = os.WriteFile(tmplFile.Name(), []byte("{{range $key, $value := .}}{{$value.ID}}.{{$value.Env.COUNTER}}{{end}}"), 0644)
	if err != nil {
		t.Errorf("Failed to write to temp file: %v\n", err)
	}

	// Create a destination file for generated output
	destFile, err := os.CreateTemp(os.TempDir(), "docker-gen-out")
	if err != nil {
		t.Errorf("Failed to create temp file: %v\n", err)
	}
	defer func() {
		destFile.Close()
		os.Remove(destFile.Name())
	}()

	// Set API version for context
	apiVersion, err := client.Version()
	if err != nil {
		t.Errorf("Failed to retrieve docker server version info: %v\n", err)
	}
	context.SetDockerEnv(apiVersion)

	// Create a filter for notifications
	notifyFilter := make(map[string][]string)
	notifyFilter["label"] = []string{"notify=true"}

	// Setup the generator with Swarm endpoint
	generator := &generator{
		Client:        client,
		SwarmClient:   client, // Use same mock for both clients
		Endpoint:      serverURL,
		SwarmEndpoint: serverURL,
		Configs: config.ConfigFile{
			Config: []config.Config{
				{
					Template:               tmplFile.Name(),
					Dest:                   destFile.Name(),
					Watch:                  true,
					Wait:                   &config.Wait{Min: 100 * time.Millisecond, Max: 200 * time.Millisecond},
					NotifyContainersFilter: notifyFilter,
					NotifyContainersSignal: 1, // SIGHUP
				},
			},
		},
		retry: false,
	}

	// Run container notification test
	generator.sendSignalToFilteredContainers(generator.Configs.Config[0])
	
	// Verify signal was sent
	if !signalSent.Load() {
		t.Error("Expected signal to be sent to container, but it wasn't")
	}
	
	// Test GetSwarmContainers
	containers, err := generator.GetSwarmContainers()
	if err != nil {
		t.Errorf("Failed to get Swarm containers: %v", err)
	}
	
	if len(containers) == 0 {
		t.Error("Expected at least one container from GetSwarmContainers")
	}
	
	// Verify the container details
	if len(containers) > 0 {
		if containers[0].ID != swarmContainerID {
			t.Errorf("Expected container ID %s, got %s", swarmContainerID, containers[0].ID)
		}
		
		// The Node ID should be from the task data instead
		if containers[0].Node.ID != "nodeabc123" {
			t.Errorf("Expected node ID nodeabc123, got %s", containers[0].Node.ID)
		}
	}
	
	// Test template generation with Swarm containers
	changed := template.GenerateFile(generator.Configs.Config[0], containers)
	if !changed {
		t.Errorf("Expected template generation to report changes")
	}
	
	// Read generated content and verify
	content, err := os.ReadFile(destFile.Name())
	if err != nil {
		t.Errorf("Failed to read generated file: %v", err)
	}
	
	expected := fmt.Sprintf("%s.%d", swarmContainerID, counter.Load())
	if string(content) != expected {
		t.Errorf("Expected content: %s, got: %s", expected, string(content))
	}
}
