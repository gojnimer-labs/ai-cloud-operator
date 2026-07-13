/*
Copyright 2026 gojnimer-labs.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package podexec runs commands inside already-running pods on behalf of
// catalog.CustomFunction implementations (see internal/catalog.PodExecutor)
// — the operator's own equivalent of `kubectl exec`, using client-go's SPDY
// executor directly rather than shelling out to a kubectl binary.
package podexec

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// workloadNameLabel must stay in sync with internal/controller's labelName
// constant — every object the reconciler creates (Deployment, and
// transitively its Pods) carries it, so it's how we find "the" pod for a
// workload without the reconciler needing to expose anything new.
const workloadNameLabel = "app.kubernetes.io/name"

// Executor implements catalog.PodExecutor using client-go's SPDY exec — the
// same mechanism `kubectl exec` uses, authenticated with the same
// *rest.Config the manager already uses for every other client-go call.
type Executor struct {
	clientset kubernetes.Interface
	config    *rest.Config
}

// New builds an Executor from cfg, the same *rest.Config the manager
// already uses.
func New(cfg *rest.Config) (*Executor, error) {
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes clientset: %w", err)
	}
	return &Executor{clientset: clientset, config: cfg}, nil
}

// Exec implements catalog.PodExecutor.
func (e *Executor) Exec(ctx context.Context, namespace, podName, container string, command []string) (stdout, stderr string, err error) {
	req := e.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")
	req.VersionedParams(&corev1.PodExecOptions{
		Command:   command,
		Container: container,
		Stdout:    true,
		Stderr:    true,
	}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(e.config, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("building spdy executor: %w", err)
	}

	var outBuf, errBuf bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &outBuf,
		Stderr: &errBuf,
	})
	return outBuf.String(), errBuf.String(), err
}

// FindPod returns the name of a running pod for workloadName in namespace,
// using the label every object the reconciler creates already carries. An
// error means "no running pod yet" — callers should surface this as such
// rather than a generic 500 (see internal/api/server.go's functions route).
func FindPod(ctx context.Context, k8sClient client.Client, namespace, workloadName string) (string, error) {
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(labels.Set{
			workloadNameLabel: workloadName,
		})},
	); err != nil {
		return "", fmt.Errorf("listing pods: %w", err)
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning {
			return pod.Name, nil
		}
	}
	return "", fmt.Errorf("no running pod found for workload %q", workloadName)
}
