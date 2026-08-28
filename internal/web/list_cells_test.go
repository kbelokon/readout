package web

import (
	"testing"
)

func TestCellViewBranchClassifier(t *testing.T) {
	tests := []struct {
		name, plural, column, label, value string
		want                               cellViewBranch
	}{
		{name: "plain", column: "Custom", want: branchPlain},
		{name: "name beats label", column: "Name", label: "app", want: branchName},
		{name: "user label", column: "Status", label: "app", want: branchLabel},
		{name: "node link", column: "Node", want: branchNode},
		{name: "node usage", plural: "nodes", column: "CPU Usage", want: branchNodeUsage},
		{name: "node capacity", plural: "nodes", column: "Memory", want: branchNodeCapacity},
		{name: "node roles", plural: "nodes", column: "Roles", want: branchNodeRoles},
		{name: "node conditions", plural: "nodes", column: "Conditions", want: branchNodeConditions},
		{name: "deployment ready", plural: "deployments", column: "Ready", want: branchDeploymentReady},
		{name: "deployment rollout", plural: "deployments", column: "Rollout", want: branchDeploymentRollout},
		{name: "namespace labels", plural: "namespaces", column: "Labels", want: branchNamespaceLabels},
		{name: "service external ip", plural: "services", column: "External-IP", want: branchPending},
		{name: "service ports", plural: "services", column: "Port(s)", want: branchServicePorts},
		{name: "service selector", plural: "services", column: "Selector", want: branchServiceSelector},
		{name: "ingress address", plural: "ingresses", column: "Address", want: branchPending},
		{name: "ingress hosts", plural: "ingresses", column: "Hosts", want: branchIngressHosts},
		{name: "ingress tls", plural: "ingresses", column: "TLS", want: branchIngressTLS},
		{name: "configmap data", plural: "configmaps", column: "Data", want: branchConfigMapData},
		{name: "secret data", plural: "secrets", column: "Data", want: branchSecretData},
		{name: "cron suspend", plural: "cronjobs", column: "Suspend", want: branchCronSuspend},
		{name: "cron last schedule", plural: "cronjobs", column: "Last Schedule", want: branchCronLastSchedule},
		{name: "job completions", plural: "jobs", column: "Completions", value: "1/2", want: branchJobCompletions},
		{name: "job non ratio", plural: "jobs", column: "Completions", value: "Complete", want: branchPlain},
		{name: "event type", plural: "events", column: "Type", want: branchEventType},
		{name: "event object", plural: "events", column: "Object", want: branchEventObject},
		{name: "event count", plural: "events", column: "Count", want: branchEventCount},
		{name: "event last seen", plural: "events", column: "Last Seen", want: branchEventLastSeen},
		{name: "event message", plural: "events", column: "Message", want: branchEventMessage},
		{name: "generic cpu", plural: "pods", column: "CPU Usage", want: branchCPUUsage},
		{name: "generic memory", plural: "pods", column: "Memory Usage", want: branchMemoryUsage},
		{name: "generic status", plural: "pods", column: "Status", want: branchStatus},
		{name: "generic ready", plural: "pods", column: "Ready", value: "1/2", want: branchReady},
		{name: "generic non ratio ready", plural: "custom", column: "Ready", value: "true", want: branchPlain},
		{name: "generic restarts", plural: "pods", column: "Restarts", want: branchRestarts},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := cellViewBranchFor(test.plural, test.column, test.label, test.value); got != test.want {
				t.Fatalf("cellViewBranchFor(%q, %q, %q, %q) = %v, want %v",
					test.plural, test.column, test.label, test.value, got, test.want)
			}
		})
	}
}
