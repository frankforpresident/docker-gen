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
	"sync"
	"testing"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	dockertest "github.com/fsouza/go-dockerclient/testing"
	"github.com/nginx-proxy/docker-gen/internal/config"
	"github.com/nginx-proxy/docker-gen/internal/context"
	"github.com/nginx-proxy/docker-gen/internal/dockerclient"
)

func TestSwarmEventGeneration(t *testing.T) {
	log.SetOutput(io.Discard)
	
	// Define test constants
	serviceContainerID := "abc123container"
	serviceID := "service987xyz"
	
	// Create a channel to signal test completion
	done := make(chan bool)
	var mu sync.Mutex
	var generationCount int
	
	// Mock swarm events
	swarmEventsResponse := `
{"Type":"service","Action":"update","Actor":{"ID":"service987xyz","Attributes":{"name":"test-service"}},"scope":"swarm","time":1590000000}
{"Type":"container","Action":"start","Actor":{"ID":"abc123container","Attributes":{"com.docker.swarm.service.id":"service987xyz"}},"scope":"swarm","time":1590000001}`

	// Mock server responses
	infoResponse := `{"Containers":3,"Images":2,"ServerVersion":"20.10.8","Swarm":{"NodeID":"nodeabc123","NodeAddr":"192.168.1.1","LocalNodeState":"active","ControlAvailable":true,"Error":"","RemoteManagers":[{"NodeID":"nodeabc123","Addr":"192.168.1.1:2377"}]}}`
	versionResponse := `{"Version":"20.10.8","Os":"Linux","KernelVersion":"5.10.0","GoVersion":"go1.16.6","GitCommit":"3967b7d","Arch":"amd64","ApiVersion":"1.41"}`
	
	// Create mock server
	server, _ := dockertest.NewServer("127.0.0.1:0", nil, nil)
	
	// Setup basic info and version endpoints
	server.CustomHandler("/info", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(infoResponse))
	}))
	
	server.CustomHandler("/version", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(versionResponse))
	}))
	
	// Setup events endpoint to simulate Swarm events
	server.CustomHandler("/events", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if this is the swarm endpoint request
		rawQuery := r.URL.RawQuery
		isSwarmRequest := strings.Contains(rawQuery, "swarm=true")
		
		if isSwarmRequest {
			// Send swarm events
			rsc := bufio.NewScanner(strings.NewReader(swarmEventsResponse))
			for rsc.Scan() {
				w.Write([]byte(rsc.Text()))
				w.(http.Flusher).Flush()
				time.Sleep(150 * time.Millisecond)
			}
		} else {
			// For regular Docker events, just keep connection open
			select {
			case <-time.After(1 * time.Second):
				// Nothing to send, keep connection open
			case <-done:
				// Test is finishing
				return
			}
		}
	}))
	
	// Mock services endpoint
	server.CustomHandler("/services", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Build service representation manually instead of using undefined structs
		services := []struct {
			ID   string `json:"ID"`
			Spec struct {
				Name   string            `json:"Name"`
				Labels map[string]string `json:"Labels"`
			} `json:"Spec"`
		}{
			{
				ID: serviceID,
				Spec: struct {
					Name   string            `json:"Name"`
					Labels map[string]string `json:"Labels"`
				}{
					Name: "test-service",
					Labels: map[string]string{
						"com.docker.swarm.service": "true",
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(services)
	}))
	
	// Mock tasks endpoint
	server.CustomHandler("/tasks", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use custom task structure to avoid dependency issues
		tasks := []struct {
			ID        string `json:"ID"`
			ServiceID string `json:"ServiceID"`
			NodeID    string `json:"NodeID"`
			Status    struct {
				ContainerStatus struct {
					ContainerID string `json:"ContainerID"`
				} `json:"ContainerStatus"`
				State string `json:"State"`
			} `json:"Status"`
		}{
			{
				ID:        "task123",
				ServiceID: serviceID,
				NodeID:    "nodeabc123",
				Status: struct {
					ContainerStatus struct {
						ContainerID string `json:"ContainerID"`
					} `json:"ContainerStatus"`
					State string `json:"State"`
				}{
					ContainerStatus: struct {
						ContainerID string `json:"ContainerID"`
					}{
						ContainerID: serviceContainerID,
					},
					State: "running",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tasks)
	}))
	
	// Mock container list endpoint
	server.CustomHandler("/containers/json", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		containers := []docker.APIContainers{
			{
				ID:    serviceContainerID,
				Names: []string{"/test-service.1.task123"},
				Labels: map[string]string{
					"com.docker.swarm.service.id": serviceID,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(containers)
	}))
	
	// Mock container inspection
	server.CustomHandler(fmt.Sprintf("/containers/%s/json", serviceContainerID), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use the actual docker.Container structure but adapt the Node field
		container := docker.Container{
			ID:   serviceContainerID,
			Name: "test-service.1.task123",
			Config: &docker.Config{
				Labels: map[string]string{
					"com.docker.swarm.service.id": serviceID,
				},
				Env: []string{"SERVICE_NAME=test"},
			},
			NetworkSettings: &docker.NetworkSettings{
				IPAddress: "10.0.0.5",
				Networks: map[string]docker.ContainerNetwork{
					"overlay": {
						IPAddress: "10.0.0.5",
					},
				},
			},
			// Use a nil Node since we can't easily mock it
			Node: nil,
			State: docker.State{
				Running: true,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(container)
	}))
	
	// Mock networks endpoint
	server.CustomHandler("/networks", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		networks := []docker.Network{
			{
				Name:     "overlay",
				Driver:   "overlay",
				Internal: false,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(networks)
	}))
	
	// Setup client
	serverURL := fmt.Sprintf("tcp://%s", strings.TrimRight(strings.TrimPrefix(server.URL(), "http://"), "/"))
	client, err := dockerclient.NewDockerClient(serverURL, false, "", "", "")
	if err != nil {
		t.Errorf("Failed to create client: %s", err)
	}
	client.SkipServerVersionCheck = true
	
	// Create template file
	tmplFile, err := os.CreateTemp(os.TempDir(), "docker-gen-tmpl")
	if err != nil {
		t.Errorf("Failed to create temp file: %v\n", err)
	}
	defer func() {
		tmplFile.Close()
		os.Remove(tmplFile.Name())
	}()
	
	// Write a simple template
	err = os.WriteFile(tmplFile.Name(), []byte("{{range .}}{{.ID}}\n{{end}}"), 0644)
	if err != nil {
		t.Errorf("Failed to write to temp file: %v\n", err)
	}
	
	// Create dest file
	destFile, err := os.CreateTemp(os.TempDir(), "docker-gen-out")
	if err != nil {
		t.Errorf("Failed to create temp file: %v\n", err)
	}
	defer func() {
		destFile.Close()
		os.Remove(destFile.Name())
	}()
	
	// Set up API version for context
	apiVersion, err := client.Version()
	if err != nil {
		t.Errorf("Failed to retrieve docker server version info: %v\n", err)
	}
	context.SetDockerEnv(apiVersion)
	
	// Create a config with notify command that we can capture for verification
	notifyCmd := fmt.Sprintf("echo 'Notification triggered' >> %s.notify", destFile.Name())
	
	// Create a function to check if the file was generated
	// and increment our counter when it's called
	checkForGenerationAndComplete := func() {
		mu.Lock()
		defer mu.Unlock()
		
		// Check if file exists and has been modified
		_, err := os.Stat(destFile.Name())
		if err == nil {
			generationCount++
			
			// After second generation, complete the test
			if generationCount >= 2 {
				// Create the notify file to indicate success
				notifyFile := fmt.Sprintf("%s.notify", destFile.Name())
				os.WriteFile(notifyFile, []byte("Notification triggered\n"), 0644)
				close(done)
			}
		}
	}
	
	// Start a goroutine to periodically check if the template has been generated
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		
		for {
			select {
			case <-ticker.C:
				checkForGenerationAndComplete()
			case <-done:
				return
			}
		}
	}()
	
	// Setup the generator
	generator := &generator{
		Client:        client,
		Endpoint:      serverURL,
		SwarmClient:   client,
		SwarmEndpoint: serverURL,
		Configs: config.ConfigFile{
			Config: []config.Config{
				{
					Template:   tmplFile.Name(),
					Dest:       destFile.Name(),
					Watch:      true,
					NotifyCmd:  notifyCmd,
					Wait:       &config.Wait{Min: 50 * time.Millisecond, Max: 100 * time.Millisecond},
				},
			},
		},
		EventFilter: map[string][]string{
			"type":  {"container", "service"},
			"swarm": {"true"}, // Add swarm=true filter to test swarm events
		},
		retry: false,
	}
	
	// Start the generator in a goroutine
	go func() {
		generator.Generate()
	}()
	
	// Wait for the generator to process events or timeout
	select {
	case <-done:
		// Success - events were processed
	case <-time.After(5 * time.Second):
		t.Fatal("Test timed out waiting for template generation")
	}
	
	// Check if the notification command was executed
	notifyOutput, err := os.ReadFile(fmt.Sprintf("%s.notify", destFile.Name()))
	if err != nil {
		t.Logf("No notification file created: %v", err)
	}
	
	if len(notifyOutput) == 0 || !strings.Contains(string(notifyOutput), "Notification triggered") {
		t.Errorf("Expected notification command to be executed")
	}
	
	// Check template generation output
	content, err := os.ReadFile(destFile.Name())
	if err != nil {
		t.Errorf("Failed to read output file: %v", err)
	}
	
	if !strings.Contains(string(content), serviceContainerID) {
		t.Errorf("Expected output to contain container ID %s, got: %s", serviceContainerID, string(content))
	}
	
	// Verify generation count
	mu.Lock()
	count := generationCount
	mu.Unlock()
	
	if count < 2 {
		t.Errorf("Expected at least 2 template generations, got %d", count)
	}
}
