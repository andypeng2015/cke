package mtest

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "github.com/coreos/bbolt"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("Kubernetes", func() {
	BeforeEach(func() {
		_, stderr, err := kubectl("create", "namespace", "mtest")
		Expect(err).NotTo(HaveOccurred(), "stderr: %s", stderr)
		_, stderr, err = kubectl("apply", "-f", "./mtest-policy.yml")
		Expect(err).NotTo(HaveOccurred(), "stderr: %s", stderr)
	})

	AfterEach(func() {
		_, stderr, err := kubectl("delete", "namespace", "mtest")
		Expect(err).NotTo(HaveOccurred(), "stderr: %s", stderr)
	})

	It("can run Pods", func() {
		By("waiting the default service account gets created")
		Eventually(func() error {
			_, stderr, err := kubectl("get", "sa/default", "-o", "json")
			if err != nil {
				return fmt.Errorf("%v: stderr=%s", err, stderr)
			}
			return nil
		}).Should(Succeed())

		By("running nginx")
		_, stderr, err := kubectl("apply", "-f", "./nginx.yml")
		Expect(err).NotTo(HaveOccurred(), "stderr=%s", stderr)

		By("checking nginx pod status")
		Eventually(func() error {
			stdout, stderr, err := kubectl("get", "pods/nginx", "-n=mtest", "-o", "json")
			if err != nil {
				return fmt.Errorf("%v: stderr=%s", err, stderr)
			}

			var pod corev1.Pod
			err = json.Unmarshal(stdout, &pod)
			if err != nil {
				return err
			}

			if !pod.Spec.HostNetwork {
				return errors.New("pod is not running in host network")
			}

			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					return nil
				}
			}
			return errors.New("pod is not yet ready")
		}).Should(Succeed())
	})

	It("has cluster DNS resources", func() {
		for resource, name := range map[string]string{
			"serviceaccounts":     "cke-cluster-dns",
			"clusterroles":        "system:cluster-dns",
			"clusterrolebindings": "system:cluster-dns",
			"configmaps":          "cluster-dns",
			"deployments":         "cluster-dns",
			"services":            "cluster-dns",
		} {
			_, stderr, err := kubectl("-n", "kube-system", "get", resource+"/"+name)
			Expect(err).NotTo(HaveOccurred(), "stderr=%s", stderr)
		}

		stdout, stderr, err := kubectl("-n", "kube-system", "get", "configmaps/cluster-dns", "-o=json")
		Expect(err).NotTo(HaveOccurred(), "stderr=%s", stderr)

		configMap := new(corev1.ConfigMap)
		err = json.Unmarshal(stdout, configMap)
		Expect(err).ShouldNot(HaveOccurred())
	})

	It("resolves Service IP", func() {
		By("getting CoreDNS Pods")
		stdout, stderr, err := kubectl("get", "-n=kube-system", "pods", "--selector=cke.cybozu.com/appname=cluster-dns", "-o=json")
		Expect(err).NotTo(HaveOccurred(), "stderr: %s", stderr)

		var pods corev1.PodList
		err = json.Unmarshal(stdout, &pods)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(pods.Items).To(HaveLen(2))

		node := pods.Items[0].Spec.NodeName

		By("deploying Service resource")
		_, stderr, err = kubectl("apply", "-f", "./nginx.yml")
		Expect(err).NotTo(HaveOccurred(), "stderr=%s", stderr)

		_, stderr, err = kubectl("expose", "-n=mtest", "pod", "nginx", "--port=80")
		Expect(err).NotTo(HaveOccurred(), "stderr=%s", stderr)

		overrides := fmt.Sprintf(`{
	"apiVersion": "v1",
	"spec": { "nodeSelector": { "kubernetes.io/hostname": "%s" }}
}`, node)
		_, stderr, err = kubectl("run",
			"-n=mtest", "--image=quay.io/cybozu/ubuntu:18.04", "--overrides="+overrides+"", "--restart=Never",
			"client", "--", "sleep", "infinity")
		Expect(err).NotTo(HaveOccurred(), "stderr: %s", stderr)

		By("waiting pods are ready")
		Eventually(func() error {
			_, stderr, err = kubectl("exec", "-n=mtest", "client", "true")
			if err != nil {
				return fmt.Errorf("%v: stderr=%s", err, stderr)
			}
			return nil
		}).Should(Succeed())

		By("resolving domain names")
		Eventually(func() error {
			_, stderr, err := kubectl("exec", "-n=mtest", "client", "getent", "hosts", "nginx")
			if err != nil {
				return fmt.Errorf("%v: stderr=%s", err, stderr)
			}

			_, stderr, err = kubectl("exec", "-n=mtest", "client", "getent", "hosts", "nginx.mtest.svc.cluster.local")
			if err != nil {
				return fmt.Errorf("%v: stderr=%s", err, stderr)
			}
			return nil
		}).Should(Succeed())
	})

	It("updates unbound config", func() {
		cluster := getCluster()
		for i := 0; i < 3; i++ {
			cluster.Nodes[i].ControlPlane = true
		}

		By("updating domain name to neco.local")
		before := cluster.Options.Kubelet.Domain
		cluster.Options.Kubelet.Domain = "neco.local"
		ckecliClusterSet(cluster)
		Eventually(func() error {
			return checkCluster(cluster)
		}).Should(Succeed())

		stdout, stderr, err := kubectl("get", "-n=kube-system", "pods", "--selector=cke.cybozu.com/appname=node-dns", "-o=json")
		Expect(err).NotTo(HaveOccurred(), "stderr: %s", stderr)

		var pods corev1.PodList
		err = json.Unmarshal(stdout, &pods)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(pods.Items).NotTo(BeEmpty())
		pod := pods.Items[0]

		Eventually(func() error {
			stdout, stderr, err := kubectl("exec", "-n=kube-system", pod.Name, "-c=unbound",
				"/usr/local/unbound/sbin/unbound-control", "--",
				"-c", "/etc/unbound/unbound.conf", "list_stubs")
			if err != nil {
				return fmt.Errorf("%v: %s", err, string(stderr))
			}
			if strings.Contains(string(stdout), "neco.local. IN stub") {
				return nil
			}
			return errors.New("unbound.conf is not updated")
		}).Should(Succeed())

		cluster.Options.Kubelet.Domain = before
		ckecliClusterSet(cluster)
		Eventually(func() error {
			return checkCluster(cluster)
		}).Should(Succeed())

	})

	It("has node DNS resources", func() {
		for _, name := range []string{
			"configmaps/node-dns",
			"daemonsets/node-dns",
			"serviceaccounts/cke-node-dns",
		} {
			_, stderr, err := kubectl("-n", "kube-system", "get", name)
			Expect(err).NotTo(HaveOccurred(), "stderr=%s", stderr)
		}

		By("checking node DNS pod status")
		Eventually(func() error {
			stdout, stderr, err := kubectl("-n", "kube-system", "get", "daemonsets/node-dns", "-o", "json")
			if err != nil {
				return fmt.Errorf("%v: stderr=%s", err, stderr)
			}

			var daemonSet appsv1.DaemonSet
			err = json.Unmarshal(stdout, &daemonSet)
			if err != nil {
				return err
			}

			if daemonSet.Status.NumberReady != 5 {
				return errors.New("NumberReady is not 5")
			}

			return nil
		}).Should(Succeed())

		By("querying www.google.com using node DNS from ubuntu pod")
		_, stderr, err := kubectl("run", "-n=mtest", "--image=quay.io/cybozu/ubuntu:18.04", "--restart=Never",
			"client", "--", "sleep", "infinity")
		Expect(err).NotTo(HaveOccurred(), "stderr: %s", stderr)
		Eventually(func() error {
			_, _, err := kubectl("exec", "-n=mtest", "client", "getent", "hosts", "www.cybozu.com")
			return err
		}).Should(Succeed())
	})

	It("has kube-system/cke-etcd Service and Endpoints", func() {
		_, stderr, err := kubectl("-n", "kube-system", "get", "services/cke-etcd")
		Expect(err).NotTo(HaveOccurred(), "stderr=%s", stderr)
		_, stderr, err = kubectl("-n", "kube-system", "get", "endpoints/cke-etcd")
		Expect(err).NotTo(HaveOccurred(), "stderr=%s", stderr)
	})

	It("can backup etcd snapshot", func() {
		By("deploying local persistent volume")
		_, stderr, err := kubectl("create", "-f", "local-pv.yml")
		Expect(err).NotTo(HaveOccurred(), "stderr=%s", stderr)

		By("enabling etcd backup")
		cluster := getCluster()
		for i := 0; i < 3; i++ {
			cluster.Nodes[i].ControlPlane = true
		}
		cluster.EtcdBackup.Enabled = true
		ckecliClusterSet(cluster)
		Eventually(func() error {
			return checkCluster(cluster)
		}).Should(Succeed())

		By("getting hostIP of etcdbackup Pod")
		var hostIP string
		Eventually(func() error {
			stdout, _, err := kubectl("-n", "kube-system", "get", "pods/etcdbackup", "-o", "json")
			if err != nil {
				return err
			}
			var pod corev1.Pod
			if err := json.Unmarshal(stdout, &pod); err != nil {
				return err
			}
			hostIP = pod.Status.HostIP
			if hostIP == "" {
				return fmt.Errorf("etcdbackup pod is not scheduled: %s", pod.String())
			}
			return nil
		}).Should(Succeed())

		By("deploying cluster-dns to etcdbackup Pod running hostIP")
		clusterDNSPatch := fmt.Sprintf(`{ "spec": { "template": { "spec": { "nodeSelector": { "kubernetes.io/hostname": "%s" } } } } } }`, hostIP)
		_, stderr, err = kubectl("patch", "deployment", "cluster-dns", "-n", "kube-system", "--patch="+clusterDNSPatch)
		Expect(err).NotTo(HaveOccurred(), "stderr=%s", stderr)

		By("deploying etcdbackup CronJob to etcdbackup Pod running hostIP")
		etcdbackupPatch := fmt.Sprintf(`{"spec": { "jobTemplate": { "spec": { "template": { "spec": { "nodeSelector": { "kubernetes.io/hostname": "%s" } } } } } } }`, hostIP)
		_, stderr, err = kubectl("patch", "cronjob", "etcdbackup", "-n", "kube-system", "--patch="+etcdbackupPatch)
		Expect(err).NotTo(HaveOccurred(), "stderr=%s", stderr)

		By("checking etcd backup job status")
		Eventually(func() error {
			stdout, stderr, err := kubectl("-n", "kube-system", "get", "job", "--sort-by=.metadata.creationTimestamp", "-o", "json")
			if err != nil {
				return fmt.Errorf("%v: stderr=%s", err, stderr)
			}

			var jobs batchv1.JobList
			err = json.Unmarshal(stdout, &jobs)
			if err != nil {
				return err
			}

			if len(jobs.Items) < 1 {
				return fmt.Errorf("no etcd backup jobs, JobList: %v", jobs)
			}
			if jobs.Items[len(jobs.Items)-1].Status.Succeeded != 1 {
				return fmt.Errorf(".Succeeded is not 1, JobList: %v", jobs)
			}

			return nil
		}).Should(Succeed())

		By("checking etcd snapshot is correct")
		stdout := ckecli("etcd", "backup", "list")
		var list []string
		err = json.Unmarshal(stdout, &list)
		Expect(err).NotTo(HaveOccurred(), "stderr=%s", stderr)
		Expect(list[0]).To(ContainSubstring("snapshot-"))

		ckecli("etcd", "backup", "get", list[0])
		gzfile, err := os.Open(list[0])
		Expect(err).ShouldNot(HaveOccurred())
		defer gzfile.Close()
		zr, err := gzip.NewReader(gzfile)
		Expect(err).ShouldNot(HaveOccurred())
		defer zr.Close()

		dbfile, err := os.Create("snapshot.db")
		Expect(err).ShouldNot(HaveOccurred())
		defer func() {
			dbfile.Close()
			os.Remove(dbfile.Name())
		}()
		_, err = io.Copy(dbfile, zr)
		Expect(err).ShouldNot(HaveOccurred())
		db, err := bolt.Open(dbfile.Name(), 0400, &bolt.Options{ReadOnly: true})
		Expect(err).ShouldNot(HaveOccurred())
		defer db.Close()

		By("confirming etcdbackup CronJob is removed when etcdbackup is disabled")
		cluster.EtcdBackup.Enabled = false
		ckecliClusterSet(cluster)
		Eventually(func() error {
			return checkCluster(cluster)
		}).Should(Succeed())
	})

	It("can rotate pod log", func() {
		if containerRuntime == "docker" {
			Skip("docker doesn't support log rotation")
		}

		By("waiting the default service account gets created")
		Eventually(func() error {
			_, stderr, err := kubectl("get", "sa/default", "-o", "json")
			if err != nil {
				return fmt.Errorf("%v: stderr=%s", err, stderr)
			}
			return nil
		}).Should(Succeed())

		By("running nginx")
		_, stderr, err := kubectl("apply", "-f", "./nginx.yml")
		Expect(err).NotTo(HaveOccurred(), "stderr=%s", stderr)

		By("checking nginx pod status")
		var pod corev1.Pod
		Eventually(func() error {
			stdout, stderr, err := kubectl("get", "pods/nginx", "-n=mtest", "-o", "json")
			if err != nil {
				return fmt.Errorf("%v: stderr=%s", err, stderr)
			}

			err = json.Unmarshal(stdout, &pod)
			if err != nil {
				return err
			}

			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					return nil
				}
			}
			return errors.New("pod is not yet ready")
		}).Should(Succeed())

		for i := 0; i < 5; i++ {
			_, stderr, err = execAt(pod.Status.HostIP, "curl", pod.Status.PodIP)
			Expect(err).NotTo(HaveOccurred(), "stderr=%s", stderr)
		}

		logFile := fmt.Sprintf("%d.log", pod.Status.ContainerStatuses[0].RestartCount)
		logPath := filepath.Join("/var/log/pods", string(pod.ObjectMeta.UID), "nginx", logFile)
		pattern := fmt.Sprintf("%s.*", logPath)

		Eventually(func() error {
			_, _, err = execAt(pod.Status.HostIP, "test", "-f", logPath)
			if err != nil {
				return fmt.Errorf("log file doesn't exist")
			}
			return nil
		}).Should(Succeed())

		// kubelet rotates logfile every 10 second.
		time.Sleep(10 * time.Second)

		_, _, err = execAt(pod.Status.HostIP, "test", "-f", pattern)
		Expect(err).To(HaveOccurred(), "log file is already rotated")

		for i := 0; i < 5; i++ {
			_, _, err = execAt(pod.Status.HostIP, "curl", pod.Status.PodIP)
			Expect(err).NotTo(HaveOccurred(), "stderr=%s", stderr)
		}

		Eventually(func() error {
			_, _, err = execAt(pod.Status.HostIP, "test", "-f", pattern)
			if err != nil {
				return fmt.Errorf("log file isn't rotated")
			}
			return nil
		}).Should(Succeed())
	})

	It("can output audit log", func() {
		By("confirming journald does not have audit log")
		logs, _, err := execAt(node1, "sudo", "journalctl", "CONTAINER_NAME=kube-apiserver", "-p", "6..6", "-q")
		Expect(err).ShouldNot(HaveOccurred())
		Expect(logs).Should(BeEmpty())

		By("enabling audit log")
		cluster := getCluster()
		for i := 0; i < 3; i++ {
			cluster.Nodes[i].ControlPlane = true
		}
		cluster.Options.APIServer.AuditLogEnabled = true
		cluster.Options.APIServer.AuditLogPolicy = `apiVersion: audit.k8s.io/v1
kind: Policy
rules:
- level: Metadata`
		ckecliClusterSet(cluster)
		Eventually(func() error {
			return checkCluster(cluster)
		}).Should(Succeed())
		logs, _, err = execAt(node1, "sudo", "journalctl", "CONTAINER_NAME=kube-apiserver", "-p", "6..6", "-q")
		Expect(err).ShouldNot(HaveOccurred())
		Expect(logs).ShouldNot(BeEmpty())
		status, _, err := getClusterStatus(cluster)
		var policyFile string
		for _, v := range status.NodeStatuses[node1].APIServer.BuiltInParams.ExtraArguments {
			if strings.HasPrefix(v, "--audit-policy-file=") {
				policyFile = v
				break
			}
		}
		Expect(policyFile).ShouldNot(BeEmpty())

		By("changing audit policy")
		cluster.Options.APIServer.AuditLogPolicy = `apiVersion: audit.k8s.io/v1
kind: Policy
rules:
- level: Request`
		ckecliClusterSet(cluster)
		Eventually(func() error {
			return checkCluster(cluster)
		}).Should(Succeed())
		status, _, err = getClusterStatus(cluster)
		var currentPolicyFile string
		for _, v := range status.NodeStatuses[node1].APIServer.BuiltInParams.ExtraArguments {
			if strings.HasPrefix(v, "--audit-policy-file=") {
				currentPolicyFile = v
				break
			}
		}
		Expect(currentPolicyFile).ShouldNot(BeEmpty())
		Expect(currentPolicyFile).ShouldNot(Equal(policyFile))

		By("disabling audit log")
		cluster.Options.APIServer.AuditLogEnabled = false
		cluster.Options.APIServer.AuditLogPolicy = ""
		ckecliClusterSet(cluster)
		Eventually(func() error {
			return checkCluster(cluster)
		}).Should(Succeed())
	})

	It("updates user-defined resources", func() {
		By("set user-defined resource")
		resources := `apiVersion: v1
kind: Namespace
metadata:
  name: foo
---
apiVersion: v1
kind: ServiceAccount
metadata:
  namespace: foo
  name: sa1
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: foo
  name: pod-reader
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "watch", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: read-pods
  namespace: foo
subjects:
- kind: ServiceAccount
  name: sa1
  namespace: foo
roleRef:
  kind: Role
  name: pod-reader
  apiGroup: rbac.authorization.k8s.io
`
		ckecliWithInput(resources, "resource", "set", "-")
		defer ckecliUnsafe(resources, "resource", "delete", "-")

		cluster := getCluster()
		for i := 0; i < 3; i++ {
			cluster.Nodes[i].ControlPlane = true
		}
		Eventually(func() error {
			return checkCluster(cluster)
		}).Should(Succeed())

		By("updating user-defined resources")
		newResources := `apiVersion: v1
kind: Namespace
metadata:
  name: foo
  labels:
    test: value
`
		ckecliWithInput(newResources, "resource", "set", "-")
		defer ckecliUnsafe(newResources, "resource", "delete", "-")
		Eventually(func() error {
			return checkCluster(cluster)
		}).Should(Succeed())

		stdout, _, err := kubectl("get", "namespaces/foo", "-o", "json")
		Expect(err).ShouldNot(HaveOccurred())
		var ns corev1.Namespace
		err = json.Unmarshal(stdout, &ns)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(ns.Labels).Should(HaveKeyWithValue("test", "value"))
	})

	It("recreates user-defined resources", func() {
		By("setting original resource")
		originals := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: busybox
  namespace: mtest
  labels:
    run: busybox
spec:
  replicas: 2
  selector:
    matchLabels:
      run: busybox
  template:
    metadata:
      labels:
        run: busybox
    spec:
      containers:
        - name: busybox
          image: busybox
          imagePullPolicy: Always
          args: ["httpd", "-f", "-p", "8000", "-h", "/etc"]
          ports:
            - name: target
              containerPort: 18000
              protocol: TCP
            - name: target-udp
              containerPort: 18000
              protocol: UDP
      restartPolicy: Always
      securityContext:
        runAsUser: 10000
`

		_, stderr, err := kubectlWithInput(originals, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "stderr: %s", stderr)

		By("setting modified resource")
		modified := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: busybox
  namespace: mtest
  labels:
    run: busybox
  annotations:
    cke.cybozu.com/revision: "1"
spec:
  replicas: 2
  selector:
    matchLabels:
      run: busybox
  template:
    metadata:
      labels:
        run: busybox
    spec:
      containers:
        - name: busybox
          image: busybox
          imagePullPolicy: Always
          args: ["httpd", "-f", "-p", "8000", "-h", "/etc"]
          ports:
            - name: target
              containerPort: 18001
              protocol: TCP
            - name: target-udp
              containerPort: 18001
              protocol: UDP
      restartPolicy: Always
      securityContext:
        runAsUser: 10000
`
		ckecliWithInput(modified, "resource", "set", "-")
		defer ckecliUnsafe(modified, "resource", "delete", "-")

		By("changing containerPort to 18001")
		cluster := getCluster()
		for i := 0; i < 3; i++ {
			cluster.Nodes[i].ControlPlane = true
		}
		Eventually(func() error {
			return checkCluster(cluster)
		}).Should(Succeed())

		Eventually(func() error {
			stdout, _, err := kubectl("get", "-n", "mtest", "deployment", "busybox", "-o", "json")
			if err != nil {
				return err
			}
			var dep appsv1.Deployment
			err = json.Unmarshal(stdout, &dep)
			if err != nil {
				return err
			}
			ports := dep.Spec.Template.Spec.Containers[0].Ports
			if len(ports) != 2 {
				return fmt.Errorf("ports len is not 2: %v", ports)
			}
			if ports[0].ContainerPort != 18001 {
				return fmt.Errorf("port[0] is not 18001: %d", ports[0].ContainerPort)
			}
			if ports[1].ContainerPort != 18001 {
				return fmt.Errorf("port[1] is not 18001: %d", ports[1].ContainerPort)
			}

			return nil
		}).Should(Succeed())
	})
})
