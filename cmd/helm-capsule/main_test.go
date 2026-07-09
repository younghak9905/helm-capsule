package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const manifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 2
  template:
    spec:
      initContainers:
      - name: init
        image: busybox:1.36
      containers:
      - name: app
        image: nginx:1.25
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: cleaner
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: cleaner
            image: registry.k8s.io/busybox:1.29
          restartPolicy: OnFailure
---
apiVersion: batch/v1
kind: Job
metadata:
  name: hook
  annotations:
    helm.sh/hook: pre-install
spec:
  template:
    spec:
      containers:
      - name: hook
        image: alpine:3.20
      restartPolicy: Never
`

const unsupportedManifest = `apiVersion: example.com/v1
kind: Widget
metadata:
  name: custom
spec:
  controllerImage: docker.io/example/controller:1.0
`

const placeholderManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: istio-ingressgateway
  namespace: istio-ingress
spec:
  template:
    spec:
      containers:
      - name: istio-proxy
        image: auto
`

const planManifest = `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: opensearch-cluster-master
spec:
  serviceName: opensearch-cluster-master-headless
  template:
    spec:
      serviceAccountName: opensearch-sa
      containers:
      - name: opensearch
        image: opensearchproject/opensearch:3.7.0
        env:
        - name: OPENSEARCH_INITIAL_ADMIN_PASSWORD
          valueFrom:
            secretKeyRef:
              name: opensearch-admin
              key: password
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes:
      - ReadWriteOnce
      resources:
        requests:
          storage: 30Gi
---
apiVersion: v1
kind: Service
metadata:
  name: opensearch-dashboards
spec:
  ports:
  - name: http
    port: 5601
    targetPort: 5601
    protocol: TCP
`

func buildLockFromManifest(t *testing.T, rendered string) (ImageLock, []byte) {
	t.Helper()
	docs, err := parseYAMLDocuments([]byte(rendered))
	if err != nil {
		t.Fatal(err)
	}
	occurrences := collectSupportedImages(docs)
	supported := map[string]bool{}
	for _, occurrence := range occurrences {
		supported[occurrence.Path.String()] = true
	}
	unsupported := collectUnsupportedImageFields(docs, supported)
	return makeLock(occurrences, "registry.internal/platform", "linux/amd64", 4, unsupported), []byte(rendered)
}

func TestBuildLockIsUnprovenUntilDigestsExist(t *testing.T) {
	lock, rendered := buildLockFromManifest(t, manifest)
	if len(lock.Images) != 4 {
		t.Fatalf("expected 4 images, got %d", len(lock.Images))
	}
	proof := computeProof(rendered, lock, true)
	if proof.Status != StatusUnproven || proof.Reason != "target_digest_missing" {
		t.Fatalf("unexpected proof: %#v", proof)
	}
}

func TestPostRenderRewritesOnlySupportedImages(t *testing.T) {
	lock, rendered := buildLockFromManifest(t, manifest)
	for i := range lock.Images {
		lock.Images[i].TargetDigest = "sha256:" + strings.Repeat(string(rune('a'+i)), 64)
		lock.Images[i].ProofStatus = StatusProven
		lock.Images[i].Reasons = nil
	}
	docs, err := parseYAMLDocuments(rendered)
	if err != nil {
		t.Fatal(err)
	}
	rewritten, applyErrors := applyLockToDocs(docs, lock, true)
	if len(applyErrors) > 0 {
		t.Fatalf("apply errors: %v", applyErrors)
	}
	out, err := encodeYAMLDocuments(rewritten)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if !strings.Contains(text, "registry.internal/platform/docker.io/library/nginx:1.25@sha256:") {
		t.Fatalf("rewritten manifest missing target image:\n%s", text)
	}
	if !strings.Contains(text, "replicas: 2") {
		t.Fatalf("non-image field lost:\n%s", text)
	}
	proof := computeProof(rendered, lock, true)
	if proof.Status != StatusProven {
		t.Fatalf("expected proven, got %#v", proof)
	}
}

func TestUnlockedImageFailsProof(t *testing.T) {
	lock := ImageLock{APIVersion: "helm-capsule/v1alpha1", Kind: "ImageLock", Status: StatusProven}
	proof := computeProof([]byte(manifest), lock, true)
	if proof.Status != StatusFailed || proof.Reason != "unlocked_images_found" {
		t.Fatalf("expected unlocked image failure, got %#v", proof)
	}
}

func TestCustomResourceImageLikeFieldIsUnproven(t *testing.T) {
	lock, rendered := buildLockFromManifest(t, unsupportedManifest)
	proof := computeProof(rendered, lock, true)
	if proof.Status != StatusUnproven || proof.Reason != "unsupported_image_fields_present" {
		t.Fatalf("expected unsupported field proof, got %#v", proof)
	}
}

func TestImageAutoPlaceholderIsNotRetargeted(t *testing.T) {
	lock, rendered := buildLockFromManifest(t, placeholderManifest)
	if len(lock.Images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(lock.Images))
	}
	entry := lock.Images[0]
	if entry.SourceImage != "auto" {
		t.Fatalf("expected source image auto, got %q", entry.SourceImage)
	}
	if entry.TargetImage != "" {
		t.Fatalf("auto placeholder should not be retargeted, got %q", entry.TargetImage)
	}
	if !hasReason(entry.Reasons, "unresolved_image_placeholder") {
		t.Fatalf("missing unresolved placeholder reason: %#v", entry.Reasons)
	}
	proof := computeProof(rendered, lock, true)
	if proof.Status != StatusUnproven || proof.Reason != "unresolved_image_placeholder" {
		t.Fatalf("expected unresolved placeholder proof, got %#v", proof)
	}
}

func planInputByType(plan Plan, inputType string) (PlanInput, bool) {
	for _, input := range plan.RequiredInputs {
		if input.Type == inputType {
			return input, true
		}
	}
	return PlanInput{}, false
}

func TestPlanDetectsStorageAndPullSecretInputs(t *testing.T) {
	docs, err := parseYAMLDocuments([]byte(planManifest))
	if err != nil {
		t.Fatal(err)
	}
	plan := makePlan(docs, "registry-cloud-kt")
	if plan.Status != "NEEDS_INPUT" {
		t.Fatalf("expected NEEDS_INPUT, got %#v", plan)
	}
	storage, ok := planInputByType(plan, "storageClass")
	if !ok {
		t.Fatalf("missing storageClass input: %#v", plan.RequiredInputs)
	}
	if len(storage.Resources) != 1 || storage.Resources[0] != "StatefulSet/opensearch-cluster-master" {
		t.Fatalf("unexpected storage resources: %#v", storage.Resources)
	}
	pullSecret, ok := planInputByType(plan, "imagePullSecret")
	if !ok {
		t.Fatalf("missing imagePullSecret input: %#v", plan.RequiredInputs)
	}
	if pullSecret.Details["expected_secret"] != "registry-cloud-kt" {
		t.Fatalf("unexpected pull secret details: %#v", pullSecret.Details)
	}
	if len(plan.Detected.Services) != 1 || plan.Detected.Services[0].Ports[0].Port != "5601" {
		t.Fatalf("service port not detected: %#v", plan.Detected.Services)
	}
	if len(plan.Detected.SecretRefs) != 1 || plan.Detected.SecretRefs[0].Name != "opensearch-admin" {
		t.Fatalf("secret ref not detected: %#v", plan.Detected.SecretRefs)
	}
}

func TestPlanAcceptsServiceAccountPullSecret(t *testing.T) {
	rendered := planManifest + `---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: opensearch-sa
imagePullSecrets:
- name: registry-cloud-kt
`
	docs, err := parseYAMLDocuments([]byte(rendered))
	if err != nil {
		t.Fatal(err)
	}
	plan := makePlan(docs, "registry-cloud-kt")
	if _, ok := planInputByType(plan, "imagePullSecret"); ok {
		t.Fatalf("ServiceAccount pull secret should satisfy workload: %#v", plan.RequiredInputs)
	}
	if len(plan.Detected.ServiceAccounts) != 1 {
		t.Fatalf("service account not detected: %#v", plan.Detected.ServiceAccounts)
	}
}

func TestBuildCommandWritesArtifacts(t *testing.T) {
	dir := t.TempDir()
	renderedPath := filepath.Join(dir, "rendered.yaml")
	outDir := filepath.Join(dir, "capsule")
	if err := os.WriteFile(renderedPath, []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	code := commandBuild([]string{
		"--release", "demo",
		"--namespace", "apps",
		"--rendered-manifest", renderedPath,
		"--target-registry", "registry.internal/platform",
		"--out", outDir,
	})
	if code != 0 {
		t.Fatalf("build command returned %d", code)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "images.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock ImageLock
	if err := json.Unmarshal(data, &lock); err != nil {
		t.Fatal(err)
	}
	if len(lock.Images) != 4 {
		t.Fatalf("expected 4 images, got %d", len(lock.Images))
	}
	if _, err := os.Stat(filepath.Join(outDir, "post-renderer")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "images.lock.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestPlanCommandWritesArtifacts(t *testing.T) {
	dir := t.TempDir()
	renderedPath := filepath.Join(dir, "rendered.yaml")
	outDir := filepath.Join(dir, "plan")
	if err := os.WriteFile(renderedPath, []byte(planManifest), 0644); err != nil {
		t.Fatal(err)
	}
	code := commandPlan([]string{
		"--release", "opensearch",
		"--namespace", "opensearch",
		"--rendered-manifest", renderedPath,
		"--pull-secret", "registry-cloud-kt",
		"--out", outDir,
	})
	if code != 0 {
		t.Fatalf("plan command returned %d", code)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Status != "NEEDS_INPUT" {
		t.Fatalf("expected NEEDS_INPUT, got %#v", plan)
	}
	if _, err := os.Stat(filepath.Join(outDir, "plan.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "rendered.plan.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalHelpListsWorkflowAndCompletion(t *testing.T) {
	text := globalHelpText()
	if !strings.Contains(text, "Workflow:") {
		t.Fatalf("global help missing workflow:\n%s", text)
	}
	if !strings.Contains(text, "completion") {
		t.Fatalf("global help missing completion command:\n%s", text)
	}
}

func TestCommandHelpIncludesExamples(t *testing.T) {
	text, ok := commandHelpText("build")
	if !ok {
		t.Fatal("build help not found")
	}
	if !strings.Contains(text, "Example:") || !strings.Contains(text, "--target-registry") {
		t.Fatalf("build help missing useful content:\n%s", text)
	}
}

func TestBashCompletionIncludesCommands(t *testing.T) {
	script, err := completionScript("bash")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"plan", "build", "mirror", "completion"} {
		if !strings.Contains(script, command) {
			t.Fatalf("completion missing %q:\n%s", command, script)
		}
	}
	if !strings.Contains(script, "complete -F _helm_capsule helm-capsule") {
		t.Fatalf("bash completion registration missing:\n%s", script)
	}
}

func TestBuildCommandAcceptsChartBeforeFlags(t *testing.T) {
	dir := t.TempDir()
	renderedPath := filepath.Join(dir, "rendered.yaml")
	outDir := filepath.Join(dir, "capsule")
	if err := os.WriteFile(renderedPath, []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	code := commandBuild([]string{
		"istio/gateway",
		"--release", "istio-ingressgateway",
		"--namespace", "istio-ingress",
		"--target-registry", "registry.internal/platform",
		"--platform", "linux/amd64",
		"--kube-version", "1.34.3",
		"--rendered-manifest", renderedPath,
		"--out", outDir,
	})
	if code != 0 {
		t.Fatalf("build command returned %d", code)
	}
	if _, err := os.Stat(filepath.Join(outDir, "images.lock.json")); err != nil {
		t.Fatal(err)
	}
}

func TestMirrorCommandAcceptsLockfileBeforeFlags(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "images.lock.json")
	lock := ImageLock{
		APIVersion: "helm-capsule/v1alpha1",
		Kind:       "ImageLock",
		Status:     StatusUnproven,
		Images: []ImageEntry{{
			SourceImage: "registry.istio.io/release/pilot:1.30.2",
			TargetImage: "registry.cloud.kt.com/registry.istio.io/release/pilot:1.30.2",
			Platform:    "linux/amd64",
		}},
	}
	if err := writeLock(lock, lockPath); err != nil {
		t.Fatal(err)
	}
	code := commandMirror([]string{filepath.Join(dir, "images.lock.yaml"), "--dry-run"})
	if code != 0 {
		t.Fatalf("mirror command returned %d", code)
	}
}

func TestExportCommandAcceptsCapsuleBeforeFlags(t *testing.T) {
	dir := t.TempDir()
	capsuleDir := filepath.Join(dir, "capsule")
	if err := os.MkdirAll(capsuleDir, 0755); err != nil {
		t.Fatal(err)
	}
	lock := ImageLock{
		APIVersion: "helm-capsule/v1alpha1",
		Kind:       "ImageLock",
		Status:     StatusProven,
	}
	if err := writeLock(lock, filepath.Join(capsuleDir, "images.lock.json")); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "capsule.tar")
	code := commandExport([]string{capsuleDir, "--output", output, "--metadata-only"})
	if code != 0 {
		t.Fatalf("export command returned %d", code)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatal(err)
	}
}
