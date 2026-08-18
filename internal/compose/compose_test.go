package compose

import "testing"

func TestParsePSHandlesNDJSON(t *testing.T) {
	out := `{"Name":"app-web-1","Service":"web","State":"running","Status":"Up 2 hours (healthy)","Health":"healthy","Image":"ghcr.io/acme/web:latest"}
{"Name":"pmon-database-1","Service":"database","State":"running","Status":"Up 2 hours","Image":"postgres:17"}`

	containers, err := parsePS(out)
	if err != nil {
		t.Fatalf("parsePS: %v", err)
	}
	if len(containers) != 2 {
		t.Fatalf("got %d containers, want 2", len(containers))
	}
	if !containers[0].Running() || !containers[0].Healthy() {
		t.Errorf("web should be running and healthy: %+v", containers[0])
	}
	if containers[1].Health != "" || !containers[1].Healthy() {
		t.Errorf("a container with no healthcheck counts as healthy: %+v", containers[1])
	}
}

func TestParsePSHandlesJSONArray(t *testing.T) {
	out := `[{"Name":"a-web-1","Service":"web","State":"running","Status":"Up (unhealthy)"}]`

	containers, err := parsePS(out)
	if err != nil {
		t.Fatalf("parsePS: %v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(containers))
	}
	if containers[0].Health != "unhealthy" {
		t.Errorf("health = %q, want unhealthy from the status string", containers[0].Health)
	}
	if containers[0].Healthy() {
		t.Error("an unhealthy container must not report healthy")
	}
}

func TestParsePSSurfacesExitedContainers(t *testing.T) {
	out := `{"Name":"stack-app-1","Service":"app","State":"exited","Status":"Exited (7) 2 seconds ago","ExitCode":7,"Image":"smoke:local"}`

	containers, err := parsePS(out)
	if err != nil {
		t.Fatalf("parsePS: %v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(containers))
	}
	if containers[0].Running() {
		t.Error("an exited container must not report running")
	}
	if !containers[0].Terminal() {
		t.Error("an exited container must report terminal so a crash is caught")
	}
}

func TestParsePSEmpty(t *testing.T) {
	containers, err := parsePS("   \n ")
	if err != nil {
		t.Fatalf("parsePS: %v", err)
	}
	if len(containers) != 0 {
		t.Fatalf("got %d containers, want 0", len(containers))
	}
}

func TestHealthFromStatus(t *testing.T) {
	cases := map[string]string{
		"Up 2 hours (healthy)":            "healthy",
		"Up 3 seconds (health: starting)": "starting",
		"Up 1 minute (unhealthy)":         "unhealthy",
		"Up 4 days":                       "",
		"Exited (1) 2 minutes ago":        "",
	}
	for status, want := range cases {
		if got := healthFromStatus(status); got != want {
			t.Errorf("healthFromStatus(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestTerminalStates(t *testing.T) {
	for _, state := range []string{"exited", "dead", "Exited"} {
		if !(Container{State: state}).Terminal() {
			t.Errorf("%q should be terminal", state)
		}
	}
	for _, state := range []string{"running", "restarting", "created"} {
		if (Container{State: state}).Terminal() {
			t.Errorf("%q should not be terminal", state)
		}
	}
}

func TestAppendServicesUsesSeparator(t *testing.T) {
	got := appendServices([]string{"compose", "pull"}, []string{"web", "db"})
	want := []string{"compose", "pull", "--", "web", "db"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	if bare := appendServices([]string{"compose", "pull"}, nil); len(bare) != 2 {
		t.Errorf("no services should not add a separator, got %v", bare)
	}
}
