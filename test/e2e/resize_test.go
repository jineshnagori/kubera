//go:build e2e
// +build e2e

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

package e2e

import (
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/jineshnagori/kubera/test/utils"
)

const workloadNamespace = "kubera-e2e-workload"

// The workload starts intentionally oversized (400m CPU request for an idle
// nginx). With metrics-server reporting near-zero usage, KubeRA must resize
// the Pod DOWN in place, step-capped, without restarting it.
var _ = Describe("In-place resize", Ordered, func() {
	BeforeAll(func() {
		By("installing metrics-server")
		cmd := exec.Command("kubectl", "apply", "-f",
			"https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install metrics-server")

		// kind nodes use self-signed kubelet certs.
		By("patching metrics-server for kind (insecure kubelet TLS)")
		cmd = exec.Command("kubectl", "-n", "kube-system", "patch", "deployment", "metrics-server",
			"--type=json", "-p",
			`[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]`)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		cmd = exec.Command("kubectl", "-n", "kube-system", "rollout", "status",
			"deployment/metrics-server", "--timeout=180s")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "metrics-server did not become ready")

		By("creating the workload namespace")
		cmd = exec.Command("kubectl", "create", "ns", workloadNamespace)
		_, _ = utils.Run(cmd)

		By("creating an oversized idle workload")
		applyStdin(oversizedDeployment)
		cmd = exec.Command("kubectl", "-n", workloadNamespace, "rollout", "status",
			"deployment/idle-api", "--timeout=120s")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("creating the DynamicResource in InPlaceOnly mode")
		applyStdin(dynamicResource)
	})

	AfterAll(func() {
		cmd := exec.Command("kubectl", "delete", "ns", workloadNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	It("publishes recommendations from live metrics", func() {
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "-n", workloadNamespace, "get", "dynamicresource",
				"idle-api-policy", "-o", "jsonpath={.status.recommendations[0].containers[0].target.cpu}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).NotTo(BeEmpty())
		}, 5*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("resizes the pod down in place without a restart", func() {
		var podName string
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "-n", workloadNamespace, "get", "pods",
				"-l", "app=idle-api", "-o", "jsonpath={.items[0].metadata.name}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			podName = strings.TrimSpace(out)
			g.Expect(podName).NotTo(BeEmpty())
		}, time.Minute, 5*time.Second).Should(Succeed())

		// Idle usage plus 10% margin sits at the 100m floor; the first
		// step-capped resize from 400m lands at 320m (20% down-step).
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "-n", workloadNamespace, "get", "pod", podName,
				"-o", "jsonpath={.spec.containers[0].resources.requests.cpu}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("320m"))
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying the container was not restarted")
		cmd := exec.Command("kubectl", "-n", workloadNamespace, "get", "pod", podName,
			"-o", "jsonpath={.status.containerStatuses[0].restartCount}")
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(Equal("0"), "in-place resize must not restart the container")

		By("verifying the same pod instance is still running (no recreate)")
		cmd = exec.Command("kubectl", "-n", workloadNamespace, "get", "pod", podName,
			"-o", "jsonpath={.status.phase}")
		out, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(Equal("Running"))
	})
})

func applyStdin(manifest string) {
	GinkgoHelper()
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

const oversizedDeployment = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: idle-api
  namespace: kubera-e2e-workload
  labels:
    app: idle-api
spec:
  replicas: 1
  selector:
    matchLabels:
      app: idle-api
  template:
    metadata:
      labels:
        app: idle-api
    spec:
      containers:
      - name: app
        image: nginx:1.27
        resizePolicy:
        - resourceName: cpu
          restartPolicy: NotRequired
        - resourceName: memory
          restartPolicy: NotRequired
        resources:
          requests:
            cpu: 400m
            memory: 256Mi
          limits:
            cpu: 800m
            memory: 512Mi
`

const dynamicResource = `
apiVersion: autoscaling.kubera.io/v1alpha1
kind: DynamicResource
metadata:
  name: idle-api-policy
  namespace: kubera-e2e-workload
spec:
  selector:
    matchLabels:
      app: idle-api
  updateMode: InPlaceOnly
  resources:
    cpu:
      min: 100m
      max: 1000m
    memory:
      min: 128Mi
      max: 1Gi
  metrics:
    pollingInterval: 15s
  behavior:
    scaleDown:
      maxStepPercent: 20
      cooldown: 30s
`
