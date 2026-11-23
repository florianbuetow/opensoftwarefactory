package main

import (
	"net"
	"strconv"
	"strings"
)

// podSpec is the dev pod: two containers that share /workspace. Only git-helper sees the
// repository's credential, and neither container gets a Kubernetes service account token.
// The two servers get different tokens, so that whatever runs in work cannot call git-helper.
func (s *service) podSpec(p devPod, repo repository) map[string]any {
	token := func(key string) []map[string]any {
		return []map[string]any{{"name": "POD_TOKEN", "valueFrom": map[string]any{
			"secretKeyRef": map[string]any{"name": podName(p.ID), "key": key},
		}}}
	}
	workspace := map[string]any{"name": "workspace", "mountPath": "/workspace"}
	work := map[string]any{
		"name":            "work",
		"image":           s.cfg.image,
		"imagePullPolicy": s.cfg.pullPolicy,
		"command":         []string{"python", "/opt/devpod/toolserver.py"},
		"workingDir":      "/workspace",
		// The image's user 1000 has its home at /home/dev; the root filesystem stays writable
		// so that commands can use it and /tmp.
		"env": append(token("work-token"),
			map[string]any{"name": "HOME", "value": "/home/dev"},
			map[string]any{"name": "PORT", "value": strconv.Itoa(workPort)}),
		"ports": []map[string]any{{"name": "tools", "containerPort": workPort}},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "100m", "memory": "128Mi"},
			"limits":   map[string]any{"cpu": "1", "memory": "1Gi"},
		},
		"securityContext": map[string]any{
			"runAsUser": 1000, "runAsGroup": 1000, "runAsNonRoot": true, "allowPrivilegeEscalation": false,
			"capabilities": map[string]any{"drop": []string{"ALL"}},
		},
		"volumeMounts": []map[string]any{workspace},
	}
	helperMounts := []map[string]any{workspace}
	volumes := []map[string]any{{"name": "workspace", "emptyDir": map[string]any{}}}
	if repo.Credential != "none" {
		helperMounts = append(helperMounts, map[string]any{"name": "credential", "mountPath": "/credential", "readOnly": true})
		volumes = append(volumes, map[string]any{"name": "credential", "secret": map[string]any{
			"secretName": credentialSecret(repo.Name), "defaultMode": 0o400,
			"items": []map[string]any{{"key": "token", "path": "token"}},
		}})
	}
	helper := map[string]any{
		"name":            "git-helper",
		"image":           s.cfg.image,
		"imagePullPolicy": s.cfg.pullPolicy,
		"command":         []string{"python", "/opt/devpod/githelper.py"},
		"workingDir":      "/workspace",
		"env":             append(token("helper-token"), map[string]any{"name": "PORT", "value": strconv.Itoa(helperPort)}),
		"ports":           []map[string]any{{"name": "git", "containerPort": helperPort}},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "25m", "memory": "64Mi"},
			"limits":   map[string]any{"cpu": "500m", "memory": "512Mi"},
		},
		// Root, but only with what it takes to change the owner and mode of files of user 1000
		// and to run git on the workspace as user 1000.
		"securityContext": map[string]any{
			"runAsUser": 0, "runAsGroup": 0, "runAsNonRoot": false, "allowPrivilegeEscalation": false,
			"capabilities": map[string]any{"drop": []string{"ALL"}, "add": []string{"CHOWN", "FOWNER", "DAC_OVERRIDE", "SETUID", "SETGID"}},
		},
		"volumeMounts": helperMounts,
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": podName(p.ID), "namespace": s.cfg.podNamespace,
			"labels":      map[string]string{"app": "devpod", "devpod-id": p.ID},
			"annotations": map[string]string{"opensoftwarefactory/repository": p.Repository, "opensoftwarefactory/work-branch": p.WorkBranch},
		},
		"spec": map[string]any{
			"automountServiceAccountToken":  false,
			"enableServiceLinks":            false,
			"restartPolicy":                 "Always",
			"terminationGracePeriodSeconds": 5,
			"securityContext":               map[string]any{"seccompProfile": map[string]any{"type": "RuntimeDefault"}},
			// Names of factory services, such as the demo Git host, resolve as they do in the
			// factory's own namespace.
			"dnsConfig":  map[string]any{"searches": []string{s.cfg.factoryNamespace + ".svc.cluster.local"}},
			"containers": []map[string]any{work, helper},
			"volumes":    volumes,
		},
	}
}

// networkPolicy lets the pod reach DNS and its repository's host only, and be reached by
// the connector service only. Both containers share the pod's network.
func (s *service) networkPolicy(id string, repo repository) map[string]any {
	tcp := func(port int) map[string]any { return map[string]any{"protocol": "TCP", "port": port} }
	egress := []map[string]any{{"ports": []map[string]any{{"protocol": "UDP", "port": 53}, tcp(53)}}}
	if rule := s.repositoryEgress(repo); rule != nil {
		egress = append(egress, rule)
	}
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{
			"name": podName(id), "namespace": s.cfg.podNamespace,
			"labels": map[string]string{"app": "devpod", "devpod-id": id},
		},
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]string{"devpod-id": id}},
			"policyTypes": []string{"Ingress", "Egress"},
			"ingress": []map[string]any{{
				"from": []map[string]any{{
					"namespaceSelector": map[string]any{"matchLabels": map[string]string{"kubernetes.io/metadata.name": s.cfg.factoryNamespace}},
					"podSelector":       map[string]any{"matchLabels": map[string]string{"app.kubernetes.io/name": "connectors"}},
				}},
				"ports": []map[string]any{tcp(workPort), tcp(helperPort)},
			}},
			"egress": egress,
		},
	}
}

// repositoryEgress is the rule that opens the way to the repository's host. A service of the
// cluster (a name without a dot, or name.namespace[.svc...]) is matched by the pods that carry
// its name as app.kubernetes.io/name; any other host by the addresses it resolves to now.
func (s *service) repositoryEgress(repo repository) map[string]any {
	host, port, err := repositoryHost(repo.URL)
	if err != nil {
		return nil
	}
	labels := strings.Split(host, ".")
	inCluster := len(labels) == 1 || (len(labels) >= 3 && labels[2] == "svc") || (len(labels) == 2 && net.ParseIP(host) == nil && labels[1] == s.cfg.factoryNamespace)
	if inCluster && net.ParseIP(host) == nil {
		namespace := s.cfg.factoryNamespace
		if len(labels) > 1 {
			namespace = labels[1]
		}
		return map[string]any{"to": []map[string]any{{
			"namespaceSelector": map[string]any{"matchLabels": map[string]string{"kubernetes.io/metadata.name": namespace}},
			"podSelector":       map[string]any{"matchLabels": map[string]string{"app.kubernetes.io/name": labels[0]}},
		}}}
	}
	addresses := []string{host}
	if net.ParseIP(host) == nil {
		if addresses, err = s.resolve(host); err != nil || len(addresses) == 0 {
			s.log.Warn("the repository's host does not resolve; the dev pod gets no way out to it", "host", host, "error", err)
			return nil
		}
	}
	to := []map[string]any{}
	for _, address := range addresses {
		ip := net.ParseIP(address)
		if ip == nil {
			continue
		}
		bits := "/128"
		if ip.To4() != nil {
			bits = "/32"
		}
		to = append(to, map[string]any{"ipBlock": map[string]any{"cidr": ip.String() + bits}})
	}
	number, _ := strconv.Atoi(port)
	return map[string]any{"to": to, "ports": []map[string]any{{"protocol": "TCP", "port": number}}}
}
