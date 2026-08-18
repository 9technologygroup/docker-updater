package main

import "testing"

// The root agent must not log into a directory the unprivileged account owns.
func TestAgentLogPathIsSeparateFromTheService(t *testing.T) {
	cases := map[string]string{
		"/var/log/dup/dup.log": "/var/log/dup-agent/dup-agent.log",
		"/opt/logs/dup.log":    "/opt/logs-agent/dup-agent.log",
		"/var/log/dup/dup":     "/var/log/dup-agent/dup-agent.log",
	}
	for in, want := range cases {
		if got := agentLogPath(in); got != want {
			t.Errorf("agentLogPath(%q) = %q, want %q", in, got, want)
		}
	}
}
