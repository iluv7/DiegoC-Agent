package workspace

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"diegoc-agent/internal/backend"
)

const k8sInstructions = `## Execution Environment
File and shell tools execute inside Kubernetes pod {pod} (namespace {namespace}, container {container}) at {workdir}. The pod has its own filesystem; diff and rewind operate on host files and are unavailable for this workspace.`

// K8sOptions configures a pod-backed workspace.
type K8sOptions struct {
	// ID is a stable identifier; defaults to the pod name.
	ID string
	// Kubeconfig and Context override kubectl's default kubeconfig and
	// current context.
	Kubeconfig string
	Context    string
	// Namespace, Pod and Container locate the workload. Defaults:
	// "default", "diegoc-agent-workspace", "agent".
	Namespace string
	Pod       string
	Container string
	// Image is used only when the pod has to be created.
	Image string
	// Workdir is the in-pod workspace root and the containers working
	// directory. Defaults to "/workspace".
	Workdir string
	// PersistentVolumeClaim, when set, mounts an existing PVC at Workdir
	// instead of an emptyDir. The claim must already exist in Namespace.
	PersistentVolumeClaim string
	// ImagePullPolicy defaults to "IfNotPresent".
	ImagePullPolicy string
	// RemoveOnClose deletes the pod when the workspace closes. The
	// default keeps it so files survive restarts.
	RemoveOnClose bool
	// StartTimeout bounds pod creation/readiness. Defaults to 120s.
	StartTimeout time.Duration
	// Runner overrides the kubectl CLI invocation; nil uses the real CLI.
	Runner backend.KubectlRunner
}

// K8sWorkspace runs tools inside a Kubernetes pod through kubectl.
type K8sWorkspace struct {
	*Base
	namespace     string
	pod           string
	container     string
	image         string
	pvc           string
	imagePull     string
	kubeconfig    string
	kubeContext   string
	removeOnClose bool
	startTimeout  time.Duration
	pollInterval  time.Duration
	prefix        []string
	runner        backend.KubectlRunner
}

var (
	_ Workspace       = (*K8sWorkspace)(nil)
	_ BackendProvider = (*K8sWorkspace)(nil)
)

// NewK8s returns a pod-backed workspace. The cluster is not touched until
// Initialize.
func NewK8s(opts K8sOptions) *K8sWorkspace {
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	if opts.Pod == "" {
		opts.Pod = "diegoc-agent-workspace"
	}
	if opts.Container == "" {
		opts.Container = "agent"
	}
	if opts.Image == "" {
		opts.Image = "debian:bookworm-slim"
	}
	if opts.Workdir == "" {
		opts.Workdir = "/workspace"
	}
	if opts.ImagePullPolicy == "" {
		opts.ImagePullPolicy = "IfNotPresent"
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = 120 * time.Second
	}
	if opts.ID == "" {
		opts.ID = opts.Pod
	}
	if opts.Runner == nil {
		opts.Runner = backend.RunKubectlCLI
	}

	w := &K8sWorkspace{
		namespace:     opts.Namespace,
		pod:           opts.Pod,
		container:     opts.Container,
		image:         opts.Image,
		pvc:           opts.PersistentVolumeClaim,
		imagePull:     opts.ImagePullPolicy,
		kubeconfig:    opts.Kubeconfig,
		kubeContext:   opts.Context,
		removeOnClose: opts.RemoveOnClose,
		startTimeout:  opts.StartTimeout,
		pollInterval:  500 * time.Millisecond,
		runner:        opts.Runner,
	}
	if opts.Kubeconfig != "" {
		w.prefix = append(w.prefix, "--kubeconfig", opts.Kubeconfig)
	}
	if opts.Context != "" {
		w.prefix = append(w.prefix, "--context", opts.Context)
	}
	instructions := strings.NewReplacer(
		"{pod}", opts.Pod,
		"{namespace}", opts.Namespace,
		"{container}", opts.Container,
	).Replace(k8sInstructions)
	w.Base = NewBase(
		opts.ID,
		opts.Workdir,
		backend.PosixPaths,
		w,
		formatInstructions(instructions, opts.Workdir),
	)
	return w
}

// Provision reuses, recreates or creates the pod, waits for Running, then
// binds a K8sBackend and installs the canonical layout.
func (w *K8sWorkspace) Provision(ctx context.Context) (backend.Backend, error) {
	if err := w.validate(); err != nil {
		return nil, err
	}
	if err := w.ensurePod(ctx); err != nil {
		return nil, err
	}

	be := backend.NewK8s(backend.K8sOptions{
		Namespace:  w.namespace,
		Pod:        w.pod,
		Container:  w.container,
		Workdir:    w.Workdir(),
		Kubeconfig: w.kubeconfig,
		Context:    w.kubeContext,
		Runner:     w.runner,
	})
	if err := w.ensureLayoutWith(ctx, be); err != nil {
		return nil, err
	}
	return be, nil
}

// Teardown optionally deletes the pod; by default it is left running so the
// next run reattaches to the same files.
func (w *K8sWorkspace) Teardown(ctx context.Context) error {
	if !w.removeOnClose {
		return nil
	}
	_, stderr, exitCode, err := w.kubectl(ctx, nil,
		"delete", "pod", w.pod, "-n", w.namespace, "--ignore-not-found",
	)
	if err != nil {
		return err
	}
	if exitCode != 0 && !isNotFound(stderr) {
		return fmt.Errorf("kubectl delete pod: %s", strings.TrimSpace(string(stderr)))
	}
	return nil
}

// ── pod lifecycle ──────────────────────────────────────────────────────

func (w *K8sWorkspace) ensurePod(ctx context.Context) error {
	phase, exists, err := w.podPhase(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return w.createAndWait(ctx)
	}
	switch phase {
	case "Running":
		return nil
	case "Failed", "Succeeded":
		// A terminal pod cannot be reused; replace it.
		if err := w.deletePod(ctx); err != nil {
			return err
		}
		return w.createAndWait(ctx)
	default:
		return w.waitForRunning(ctx)
	}
}

func (w *K8sWorkspace) createAndWait(ctx context.Context) error {
	manifest, err := w.manifest()
	if err != nil {
		return err
	}
	_, stderr, exitCode, err := w.kubectl(ctx, manifest, "apply", "-f", "-")
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("kubectl apply: %s", strings.TrimSpace(string(stderr)))
	}
	return w.waitForRunning(ctx)
}

func (w *K8sWorkspace) waitForRunning(ctx context.Context) error {
	deadline := time.Now().Add(w.startTimeout)
	delay := w.pollInterval
	lastPhase := "Unknown"
	for {
		phase, exists, err := w.podPhase(ctx)
		if err != nil {
			return err
		}
		if exists {
			lastPhase = phase
			if phase == "Running" {
				return nil
			}
			if phase == "Failed" || phase == "Succeeded" {
				return fmt.Errorf(
					"pod %q entered phase %s while waiting for Running",
					w.pod, phase,
				)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"pod %q did not become Running within %s (last phase %q)",
				w.pod, w.startTimeout, lastPhase,
			)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 3*time.Second {
			delay *= 2
			if delay > 3*time.Second {
				delay = 3 * time.Second
			}
		}
	}
}

func (w *K8sWorkspace) podPhase(ctx context.Context) (phase string, exists bool, err error) {
	stdout, stderr, exitCode, err := w.kubectl(ctx, nil,
		"get", "pod", w.pod, "-n", w.namespace,
		"-o", "jsonpath={.status.phase}",
	)
	if err != nil {
		return "", false, err
	}
	if exitCode != 0 {
		if isNotFound(stderr) {
			return "", false, nil
		}
		return "", false, fmt.Errorf(
			"kubectl get pod: %s",
			strings.TrimSpace(string(stderr)),
		)
	}
	return strings.TrimSpace(string(stdout)), true, nil
}

func (w *K8sWorkspace) deletePod(ctx context.Context) error {
	_, stderr, exitCode, err := w.kubectl(ctx, nil,
		"delete", "pod", w.pod, "-n", w.namespace, "--ignore-not-found",
	)
	if err != nil {
		return err
	}
	if exitCode != 0 && !isNotFound(stderr) {
		return fmt.Errorf("kubectl delete pod: %s", strings.TrimSpace(string(stderr)))
	}
	return nil
}

func (w *K8sWorkspace) kubectl(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, int, error) {
	full := make([]string, 0, len(w.prefix)+len(args))
	full = append(full, w.prefix...)
	full = append(full, args...)
	return w.runner(ctx, full, stdin)
}

// ── manifest ───────────────────────────────────────────────────────────

var k8sNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

func (w *K8sWorkspace) validate() error {
	for kind, value := range map[string]string{
		"namespace": w.namespace,
		"pod":       w.pod,
		"container": w.container,
	} {
		if !k8sNamePattern.MatchString(value) {
			return fmt.Errorf("k8s workspace: invalid %s name %q", kind, value)
		}
	}
	if w.pvc != "" && !k8sNamePattern.MatchString(w.pvc) {
		return fmt.Errorf("k8s workspace: invalid persistent volume claim %q", w.pvc)
	}
	if !strings.HasPrefix(w.Workdir(), "/") {
		return fmt.Errorf(
			"k8s workspace: workdir must be an absolute path, got %q",
			w.Workdir(),
		)
	}
	return nil
}

func (w *K8sWorkspace) manifest() ([]byte, error) {
	volume := map[string]interface{}{"name": "workspace", "emptyDir": map[string]interface{}{}}
	if w.pvc != "" {
		volume = map[string]interface{}{
			"name":                  "workspace",
			"persistentVolumeClaim": map[string]interface{}{"claimName": w.pvc},
		}
	}
	pod := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"name":      w.pod,
			"namespace": w.namespace,
			"labels": map[string]string{
				"app.kubernetes.io/name":     "diegoc-agent",
				"app.kubernetes.io/instance": w.pod,
			},
		},
		"spec": map[string]interface{}{
			"restartPolicy": "Never",
			"containers": []interface{}{
				map[string]interface{}{
					"name":            w.container,
					"image":           w.image,
					"imagePullPolicy": w.imagePull,
					"command":         []string{"sh", "-c", "while true; do sleep 3600; done"},
					"workingDir":      w.Workdir(),
					"volumeMounts": []interface{}{
						map[string]interface{}{
							"name":      "workspace",
							"mountPath": w.Workdir(),
						},
					},
				},
			},
			"volumes": []interface{}{volume},
		},
	}
	return yaml.Marshal(pod)
}

// isNotFound recognizes kubectl's missing-object errors.
func isNotFound(stderr []byte) bool {
	text := strings.ToLower(string(stderr))
	return strings.Contains(text, "notfound") || strings.Contains(text, "not found")
}
