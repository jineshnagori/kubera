/*
Copyright 2026.

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

// kubectl-kubera is a kubectl plugin. Install by placing the binary on PATH;
// then `kubectl kubera diff` shows, per managed workload, the gap between
// current requests and the KubeRA recommendation.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	autoscalingv1alpha1 "github.com/jineshnagori/kubera/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
)

func main() {
	namespace := flag.String("n", "", "namespace (default: all namespaces)")
	kubeconfig := flag.String("kubeconfig", defaultKubeconfig(), "path to kubeconfig")
	flag.Parse()

	if flag.Arg(0) != "diff" {
		fmt.Fprintln(os.Stderr, "usage: kubectl kubera diff [-n namespace]")
		os.Exit(2)
	}

	if err := runDiff(*kubeconfig, *namespace); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func defaultKubeconfig() string {
	if v := os.Getenv("KUBECONFIG"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}

func runDiff(kubeconfig, namespace string) error {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return err
	}

	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	drClient, err := dynamicResourceClient(cfg)
	if err != nil {
		return err
	}

	ctx := context.Background()
	var list autoscalingv1alpha1.DynamicResourceList
	if err := drClient.Get().Namespace(namespace).Resource("dynamicresources").Do(ctx).Into(&list); err != nil {
		return fmt.Errorf("listing dynamicresources: %w", err)
	}

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	defer func() { _ = w.Flush() }()
	header := "NAMESPACE\tWORKLOAD\tCONTAINER\tCURRENT CPU\tRECOMMENDED CPU\tCURRENT MEMORY\tRECOMMENDED MEMORY"
	_, _ = fmt.Fprintln(w, header)

	for i := range list.Items {
		dr := &list.Items[i]
		for _, rec := range dr.Status.Recommendations {
			deploy, err := kube.AppsV1().Deployments(dr.Namespace).Get(ctx, rec.Workload, metav1.GetOptions{})
			if err != nil {
				continue
			}
			current := map[string]corev1.ResourceList{}
			for _, c := range deploy.Spec.Template.Spec.Containers {
				current[c.Name] = c.Resources.Requests
			}
			for _, c := range rec.Containers {
				curCPU, curMem := "-", "-"
				if cur, ok := current[c.ContainerName]; ok {
					if q, ok := cur[corev1.ResourceCPU]; ok {
						curCPU = q.String()
					}
					if q, ok := cur[corev1.ResourceMemory]; ok {
						curMem = q.String()
					}
				}
				recCPU := c.Target[corev1.ResourceCPU]
				recMem := c.Target[corev1.ResourceMemory]
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					dr.Namespace, rec.Workload, c.ContainerName,
					curCPU, recCPU.String(), curMem, recMem.String())
			}
		}
	}
	return nil
}

// dynamicResourceClient builds a minimal REST client for the KubeRA API group.
func dynamicResourceClient(cfg *rest.Config) (rest.Interface, error) {
	scheme := runtime.NewScheme()
	if err := autoscalingv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	c := rest.CopyConfig(cfg)
	gv := schema.GroupVersion{Group: "autoscaling.kubera.io", Version: "v1alpha1"}
	c.GroupVersion = &gv
	c.APIPath = "/apis"
	c.NegotiatedSerializer = serializer.NewCodecFactory(scheme).WithoutConversion()
	return rest.RESTClientFor(c)
}
