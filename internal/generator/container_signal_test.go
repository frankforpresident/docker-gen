package generator

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	docker "github.com/fsouza/go-dockerclient"
	dockertest "github.com/fsouza/go-dockerclient/testing"
	"github.com/nginx-proxy/docker-gen/internal/config"
	"github.com/nginx-proxy/docker-gen/internal/dockerclient"
)

func TestSendSignalToContainer(t *testing.T) {
	log.SetOutput(io.Discard)
	
	// Container IDs for testing
	regularContainerID := "regular123456789"
	swarmContainerID := "swarm987654321"
	
	// Track signal calls
	var regularSignalSent atomic.Bool
	var swarmSignalSent atomic.Bool
	
	// Setup Docker API mock server
	regularServer, _ := dockertest.NewServer("127.0.0.1:0", nil, nil)
	
	// Mock container kill endpoint for regular Docker
	regularServer.CustomHandler(fmt.Sprintf("/containers/%s/kill", regularContainerID), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signal := r.URL.Query().Get("signal")
		if signal == "1" || signal == "HUP" {
			regularSignalSent.Store(true)
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	
	// Setup mock info endpoint
	infoResponse := `{"ServerVersion":"20.10.8"}`
	regularServer.CustomHandler("/info", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(infoResponse))
	}))
	
	// Get regular server URL
	regularURL := fmt.Sprintf("tcp://%s", strings.TrimRight(strings.TrimPrefix(regularServer.URL(), "http://"), "/"))
	
	// Setup Swarm API mock server (separate server to simulate different endpoints)
	swarmServer, _ := dockertest.NewServer("127.0.0.1:0", nil, nil)
	
	// Mock container kill endpoint for Swarm
	swarmServer.CustomHandler(fmt.Sprintf("/containers/%s/kill", swarmContainerID), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signal := r.URL.Query().Get("signal")
		if signal == "1" || signal == "HUP" {
			swarmSignalSent.Store(true)
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	
	// Mock Swarm info endpoint
	swarmInfoResponse := `{"ServerVersion":"20.10.8","Swarm":{"NodeID":"nodeabc123","LocalNodeState":"active","ControlAvailable":true}}`
	swarmServer.CustomHandler("/info", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(swarmInfoResponse))
	}))
	
	// Mock container inspection endpoints (needed if we do a restart instead of kill)
	containerResponse := `{"State":{"Running":true},"HostConfig":{}}`
	
	regularServer.CustomHandler(fmt.Sprintf("/containers/%s/json", regularContainerID), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(containerResponse))
	}))
	
	swarmServer.CustomHandler(fmt.Sprintf("/containers/%s/json", swarmContainerID), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(containerResponse))
	}))
	
	// Mock container restart endpoints
	regularServer.CustomHandler(fmt.Sprintf("/containers/%s/restart", regularContainerID), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		regularSignalSent.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	
	swarmServer.CustomHandler(fmt.Sprintf("/containers/%s/restart", swarmContainerID), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		swarmSignalSent.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	
	// Get swarm server URL
	swarmURL := fmt.Sprintf("tcp://%s", strings.TrimRight(strings.TrimPrefix(swarmServer.URL(), "http://"), "/"))
	
	// Create Docker clients
	regularClient, err := dockerclient.NewDockerClient(regularURL, false, "", "", "")
	if err != nil {
		t.Fatalf("Failed to create regular Docker client: %s", err)
	}
	regularClient.SkipServerVersionCheck = true
	
	swarmClient, err := dockerclient.NewDockerClient(swarmURL, false, "", "", "")
	if err != nil {
		t.Fatalf("Failed to create Swarm Docker client: %s", err)
	}
	swarmClient.SkipServerVersionCheck = true
	
	// Set up test cases
	testCases := []struct {
		name          string
		generator     *generator
		containerID   string
		signal        int
		expectedFlag  *atomic.Bool
		expectRestart bool
	}{
		{
			name: "Send HUP signal to regular container",
			generator: &generator{
				Client:       regularClient,
				Endpoint:     regularURL,
				SwarmClient:  swarmClient,
				SwarmEndpoint: "",
			},
			containerID:  regularContainerID,
			signal:       int(docker.SIGHUP),
			expectedFlag: &regularSignalSent,
		},
		{
			name: "Send HUP signal to Swarm container",
			generator: &generator{
				Client:       regularClient,
				Endpoint:     regularURL,
				SwarmClient:  swarmClient,
				SwarmEndpoint: swarmURL,
			},
			containerID:  swarmContainerID,
			signal:       int(docker.SIGHUP),
			expectedFlag: &swarmSignalSent,
		},
		{
			name: "Restart regular container",
			generator: &generator{
				Client:       regularClient,
				Endpoint:     regularURL,
				SwarmClient:  swarmClient,
				SwarmEndpoint: "",
			},
			containerID:   regularContainerID,
			signal:        -1, // special signal for restart
			expectedFlag:  &regularSignalSent,
			expectRestart: true,
		},
		{
			name: "Restart Swarm container",
			generator: &generator{
				Client:       regularClient,
				Endpoint:     regularURL,
				SwarmClient:  swarmClient,
				SwarmEndpoint: swarmURL,
			},
			containerID:   swarmContainerID,
			signal:        -1, // special signal for restart
			expectedFlag:  &swarmSignalSent,
			expectRestart: true,
		},
	}
	
	// Run test cases
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Reset signal flags
			tc.expectedFlag.Store(false)
			
			// Send the signal
			tc.generator.sendSignalToContainer(tc.containerID, tc.signal)
			
			// Verify signal was sent
			if !tc.expectedFlag.Load() {
				action := "signal"
				if tc.expectRestart {
					action = "restart"
				}
				t.Errorf("Failed to %s container %s", action, tc.containerID)
			}
		})
	}
	
	// Test sending signal to filtered containers
	t.Run("Send signal to filtered containers", func(t *testing.T) {
		// Reset signal flags
		regularSignalSent.Store(false)
		swarmSignalSent.Store(false)
		
		// Setup mock container list for filtered containers
		regularServer.CustomHandler("/containers/json", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check filter params
			query := r.URL.Query()
			labelFilter := query.Get("filters")
			
			// If label filter contains "test=true"
			if strings.Contains(labelFilter, "test=true") {
				containers := []docker.APIContainers{
					{
						ID:     regularContainerID,
						Labels: map[string]string{"test": "true"},
					},
				}
				
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(containers)
			} else {
				// Empty list
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode([]docker.APIContainers{})
			}
		}))
		
		swarmServer.CustomHandler("/containers/json", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check filter params
			query := r.URL.Query()
			labelFilter := query.Get("filters")
			
			// If label filter contains "test=true"
			if strings.Contains(labelFilter, "test=true") {
				containers := []docker.APIContainers{
					{
						ID:     swarmContainerID,
						Labels: map[string]string{"test": "true"},
					},
				}
				
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(containers)
			} else {
				// Empty list
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode([]docker.APIContainers{})
			}
		}))
		
		// Create filter
		notifyFilter := map[string][]string{
			"label": {"test=true"},
		}
		
		// Test with regular Docker
		regularGen := &generator{
			Client:       regularClient,
			Endpoint:     regularURL,
		}
		
		regularGen.sendSignalToFilteredContainers(config.Config{
			NotifyContainersFilter: notifyFilter,
			NotifyContainersSignal: int(docker.SIGHUP),
		})
		
		if !regularSignalSent.Load() {
			t.Error("Failed to send signal to filtered containers in regular Docker mode")
		}
		
		// Reset and test with Swarm
		regularSignalSent.Store(false)
		swarmSignalSent.Store(false)
		
		swarmGen := &generator{
			Client:        regularClient,
			Endpoint:      regularURL,
			SwarmClient:   swarmClient,
			SwarmEndpoint: swarmURL,
		}
		
		swarmGen.sendSignalToFilteredContainers(config.Config{
			NotifyContainersFilter: notifyFilter,
			NotifyContainersSignal: int(docker.SIGHUP),
		})
		
		if !swarmSignalSent.Load() {
			t.Error("Failed to send signal to filtered containers in Swarm mode")
		}
	})
}
