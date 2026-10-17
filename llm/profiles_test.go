package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type capturedProfileRequest struct {
	method        string
	path          string
	authorization string
	body          map[string]any
	decodeErr     error
}

func TestProfilesRouteIndependentStreamingConnections(t *testing.T) {
	const count = 4
	requestsA := make(chan capturedProfileRequest, count+1)
	requestsB := make(chan capturedProfileRequest, count)
	serverA := newProfileServer(t, requestsA, "provider-a")
	serverB := newProfileServer(t, requestsB, "provider-b")
	clientA := NewOpenAI(serverA.URL+"/v1/", "test-key-a")
	clientB := NewOpenAI(serverB.URL+"/v1/", "test-key-b")
	clients := map[string]ChatModel{"primary": clientA, "secondary": clientB}
	profiles := NewProfiles(clients)
	// The router owns its profile map; subsequent edits by its caller cannot
	// redirect conversations or credentials to another connection.
	clients["primary"] = clientB
	delete(clients, "secondary")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errors := make(chan error, count*2)
	for range count {
		for _, scenario := range []struct {
			profile, model, answer string
			thinking               bool
		}{
			{"primary", "model-a", "provider-a", false},
			{"secondary", "model-b", "provider-b", true},
		} {
			go func() {
				req := profileRequest(scenario.profile, scenario.model)
				req.EnableThinking = &scenario.thinking
				var streamed strings.Builder
				response, err := profiles.Chat(ctx, req, func(delta Delta) { streamed.WriteString(delta.Content) })
				if err == nil && (response.Message.Content != scenario.answer || streamed.String() != scenario.answer || response.FinishReason != "stop") {
					err = fmt.Errorf("%s returned content=%q, stream=%q, finish=%q", scenario.profile, response.Message.Content, streamed.String(), response.FinishReason)
				}
				errors <- err
			}()
		}
	}
	for range count * 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	for range count {
		assertProfileRequest(t, <-requestsA, "test-key-a", "model-a", false)
		assertProfileRequest(t, <-requestsB, "test-key-b", "model-b", true)
	}
	// Legacy requests without a profile use primary. Unspecified thinking
	// remains absent, rather than acquiring the previous request's setting.
	if _, err := profiles.Chat(ctx, profileRequest("", "legacy-model"), nil); err != nil {
		t.Fatal(err)
	}
	legacy := <-requestsA
	if legacy.authorization != "Bearer test-key-a" || legacy.body["model"] != "legacy-model" {
		t.Fatalf("default profile was not primary: %#v", legacy)
	}
	if _, exists := legacy.body["enable_thinking"]; exists {
		t.Fatalf("thinking leaked into request without settings: %#v", legacy.body)
	}
}

func TestUnknownProfileDoesNotMakeRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeProfileStream(w, "should not be used")
	}))
	defer server.Close()
	profiles := NewProfiles(map[string]ChatModel{"primary": NewOpenAI(server.URL+"/v1/", "test-key")})
	_, err := profiles.Chat(context.Background(), profileRequest("missing", "model"), nil)
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("unknown profile error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("unknown profile fell back to a configured endpoint: %d calls", calls.Load())
	}
}

func TestSameProfileConcurrentRequestsKeepTheirOwnModelSettings(t *testing.T) {
	const count = 18
	requests := make(chan capturedProfileRequest, count)
	server := newProfileServer(t, requests, "shared provider")
	profiles := NewProfiles(map[string]ChatModel{"shared": NewOpenAI(server.URL+"/v1/", "shared-test-key")})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errors := make(chan error, count)
	start := make(chan struct{})
	wantThinking := make(map[string]*bool, count)
	for i := range count {
		req := profileRequest("shared", fmt.Sprintf("model-%d", i))
		if i%3 != 2 {
			thinking := i%3 == 1
			req.EnableThinking = &thinking
		}
		wantThinking[req.Model] = req.EnableThinking
		go func() {
			<-start
			_, err := profiles.Chat(ctx, req, nil)
			errors <- err
		}()
	}
	close(start)
	for range count {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	for range count {
		request := <-requests
		if request.decodeErr != nil {
			t.Fatal(request.decodeErr)
		}
		if request.authorization != "Bearer shared-test-key" {
			t.Fatalf("incorrect shared connection credential: %q", request.authorization)
		}
		model, _ := request.body["model"].(string)
		thinking, exists := wantThinking[model]
		if !exists {
			t.Fatalf("unexpected or duplicate model in concurrent request: %q", model)
		}
		actual, present := request.body["enable_thinking"]
		if thinking == nil {
			if present {
				t.Fatalf("%s inherited another request's thinking setting: %#v", model, actual)
			}
		} else if !present || actual != *thinking {
			t.Fatalf("%s has thinking=%#v, want %t", model, actual, *thinking)
		}
		delete(wantThinking, model)
	}
}

func TestEmptyProfileKeyDoesNotInheritEnvironment(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "environment-key-must-not-leak")
	requests := make(chan capturedProfileRequest, 1)
	server := newProfileServer(t, requests, "local provider")
	profiles := NewProfiles(map[string]ChatModel{"local": NewOpenAI(server.URL+"/v1/", "")})
	if _, err := profiles.Chat(context.Background(), profileRequest("local", "local-model"), nil); err != nil {
		t.Fatal(err)
	}
	request := <-requests
	if request.authorization != "" {
		t.Fatalf("empty key sent Authorization %q", request.authorization)
	}
	if request.body["model"] != "local-model" {
		t.Fatalf("incorrect local model: %#v", request.body)
	}
}

func TestProviderErrorDoesNotExposeResponseOrCredentials(t *testing.T) {
	const secret = "private-key-do-not-return"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":{"message":"rejected credential %s; upstream-secret-body","type":"authentication_error"}}`, secret)
	}))
	defer server.Close()
	profiles := NewProfiles(map[string]ChatModel{
		"private": NewOpenAI(server.URL+"/private-url-component/", secret),
	})
	_, err := profiles.Chat(context.Background(), profileRequest("private", "model"), nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("expected safe status error, got %v", err)
	}
	for _, forbidden := range []string{secret, "upstream-secret-body", "private-url-component", server.URL} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("provider error exposed %q: %v", forbidden, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("401 request count = %d", calls.Load())
	}
}

func profileRequest(profileID, model string) ChatRequest {
	return ChatRequest{
		ModelProfileID: profileID,
		Model:          model,
		ToolChoice:     ToolChoiceAuto,
		Messages:       []ChatMessage{{Role: RoleUser, Content: "Say hello"}},
	}
}

func newProfileServer(t *testing.T, requests chan<- capturedProfileRequest, answer string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := capturedProfileRequest{method: r.Method, path: r.URL.Path, authorization: r.Header.Get("Authorization")}
		request.decodeErr = json.NewDecoder(r.Body).Decode(&request.body)
		requests <- request
		writeProfileStream(w, answer)
	}))
	t.Cleanup(server.Close)
	return server
}

func writeProfileStream(w http.ResponseWriter, answer string) {
	w.Header().Set("Content-Type", "text/event-stream")
	encoded, _ := json.Marshal(answer)
	fmt.Fprintf(w, "data: {\"id\":\"completion\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%s},\"finish_reason\":null}]}\n\n", encoded)
	fmt.Fprint(w, "data: {\"id\":\"completion\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
}

func assertProfileRequest(t *testing.T, request capturedProfileRequest, key, model string, thinking bool) {
	t.Helper()
	if request.decodeErr != nil {
		t.Fatal(request.decodeErr)
	}
	if request.method != http.MethodPost || request.path != "/v1/chat/completions" {
		t.Fatalf("unexpected provider route: %s %s", request.method, request.path)
	}
	if request.authorization != "Bearer "+key || request.body["model"] != model || request.body["enable_thinking"] != thinking {
		t.Fatalf("provider request crossed profile settings: auth=%q, body=%#v", request.authorization, request.body)
	}
	if request.body["stream"] != true {
		t.Fatalf("request is not streamed: %#v", request.body)
	}
}
