package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	StatusProven   = "PROVEN"
	StatusUnproven = "UNPROVEN"
	StatusFailed   = "FAILED"
)

var (
	supportedPodSpecPaths = map[string][]string{
		"Pod":                   {"spec"},
		"Deployment":            {"spec", "template", "spec"},
		"ReplicaSet":            {"spec", "template", "spec"},
		"ReplicationController": {"spec", "template", "spec"},
		"StatefulSet":           {"spec", "template", "spec"},
		"DaemonSet":             {"spec", "template", "spec"},
		"Job":                   {"spec", "template", "spec"},
		"CronJob":               {"spec", "jobTemplate", "spec", "template", "spec"},
	}
	containerFields = []string{"initContainers", "containers", "ephemeralContainers"}
	imageLikeRE     = regexp.MustCompile(`^(?:[a-z0-9]+(?:(?:[._-][a-z0-9]+)+|:[0-9]+)?/)?[a-z0-9][a-z0-9._/-]*(?::[\w][\w.-]{0,127})?(?:@sha256:[a-fA-F0-9]{64})?$`)
)

type PathStep struct {
	Key     string `json:"key,omitempty" yaml:"key,omitempty"`
	Index   int    `json:"index,omitempty" yaml:"index,omitempty"`
	IsIndex bool   `json:"isIndex" yaml:"isIndex"`
}

type Path []PathStep

type ImageOccurrence struct {
	SourceImage string
	Path        Path
	Origin      string
	Hook        bool
}

type UnsupportedField struct {
	Path  string `json:"path" yaml:"path"`
	Value string `json:"value" yaml:"value"`
}

type ImageOrigin struct {
	Path     string `json:"path" yaml:"path"`
	Resource string `json:"resource" yaml:"resource"`
	Hook     bool   `json:"hook" yaml:"hook"`
}

type ImageEntry struct {
	SourceImage  string        `json:"source_image" yaml:"source_image"`
	SourceDigest string        `json:"source_digest" yaml:"source_digest"`
	TargetImage  string        `json:"target_image" yaml:"target_image"`
	TargetDigest string        `json:"target_digest" yaml:"target_digest"`
	Platform     string        `json:"platform" yaml:"platform"`
	ArchiveTag   string        `json:"archive_tag" yaml:"archive_tag"`
	Origins      []ImageOrigin `json:"origins" yaml:"origins"`
	ProofStatus  string        `json:"proof_status" yaml:"proof_status"`
	Reasons      []string      `json:"reasons,omitempty" yaml:"reasons,omitempty"`
}

type ImageLock struct {
	APIVersion        string             `json:"apiVersion" yaml:"apiVersion"`
	Kind              string             `json:"kind" yaml:"kind"`
	Status            string             `json:"status" yaml:"status"`
	Images            []ImageEntry       `json:"images" yaml:"images"`
	UnsupportedFields []UnsupportedField `json:"unsupported_fields,omitempty" yaml:"unsupported_fields,omitempty"`
	HelmMajor         int                `json:"helm_major" yaml:"helm_major"`
}

type Proof struct {
	Status               string             `json:"status"`
	Reason               string             `json:"reason,omitempty"`
	ImageCount           int                `json:"image_count,omitempty"`
	MissingTargetDigests []string           `json:"missing_target_digests,omitempty"`
	UnsupportedFields    []UnsupportedField `json:"unsupported_fields,omitempty"`
	NonImageChanges      []string           `json:"non_image_changes,omitempty"`
	Errors               []string           `json:"errors,omitempty"`
	Images               []string           `json:"images,omitempty"`
	Error                string             `json:"error,omitempty"`
}

type stringList []string

func (s *stringList) String() string {
	return strings.Join(*s, ",")
}

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func keyStep(key string) PathStep {
	return PathStep{Key: key}
}

func indexStep(index int) PathStep {
	return PathStep{Index: index, IsIndex: true}
}

func (p Path) String() string {
	var b strings.Builder
	b.WriteString("$")
	for _, step := range p {
		if step.IsIndex {
			fmt.Fprintf(&b, "[%d]", step.Index)
		} else {
			b.WriteByte('.')
			b.WriteString(step.Key)
		}
	}
	return b.String()
}

func parseYAMLDocuments(data []byte) ([]*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(doc.Content) == 0 {
			continue
		}
		docs = append(docs, &doc)
	}
	return docs, nil
}

func encodeYAMLDocuments(docs []*yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	for _, doc := range docs {
		if err := encoder.Encode(doc); err != nil {
			return nil, err
		}
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func cloneNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		c.Content[i] = cloneNode(child)
	}
	return &c
}

func cloneDocs(docs []*yaml.Node) []*yaml.Node {
	out := make([]*yaml.Node, len(docs))
	for i, doc := range docs {
		out[i] = cloneNode(doc)
	}
	return out
}

func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc
}

func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func scalarString(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

func nodeAt(docs []*yaml.Node, path Path) (*yaml.Node, error) {
	if len(path) == 0 || !path[0].IsIndex {
		return nil, fmt.Errorf("path must start with document index: %s", path.String())
	}
	if path[0].Index < 0 || path[0].Index >= len(docs) {
		return nil, fmt.Errorf("document index out of range: %s", path.String())
	}
	cur := documentRoot(docs[path[0].Index])
	for _, step := range path[1:] {
		if step.IsIndex {
			if cur == nil || cur.Kind != yaml.SequenceNode || step.Index < 0 || step.Index >= len(cur.Content) {
				return nil, fmt.Errorf("sequence index not found at %s", path.String())
			}
			cur = cur.Content[step.Index]
			continue
		}
		cur = mapValue(cur, step.Key)
		if cur == nil {
			return nil, fmt.Errorf("map key %q not found at %s", step.Key, path.String())
		}
	}
	return cur, nil
}

func resourceLabel(resource *yaml.Node) string {
	kind := scalarString(mapValue(resource, "kind"))
	if kind == "" {
		kind = "Unknown"
	}
	metadata := mapValue(resource, "metadata")
	name := scalarString(mapValue(metadata, "name"))
	if name == "" {
		name = "unnamed"
	}
	namespace := scalarString(mapValue(metadata, "namespace"))
	if namespace != "" {
		return fmt.Sprintf("%s/%s/%s", kind, namespace, name)
	}
	return fmt.Sprintf("%s/%s", kind, name)
}

func isHelmHook(resource *yaml.Node) bool {
	metadata := mapValue(resource, "metadata")
	annotations := mapValue(metadata, "annotations")
	return scalarString(mapValue(annotations, "helm.sh/hook")) != ""
}

func iterResourceDocs(doc *yaml.Node, base Path, visit func(*yaml.Node, Path)) {
	root := documentRoot(doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return
	}
	if scalarString(mapValue(root, "kind")) == "List" {
		items := mapValue(root, "items")
		if items != nil && items.Kind == yaml.SequenceNode {
			for i, item := range items.Content {
				iterResourceDocs(item, append(append(Path{}, base...), keyStep("items"), indexStep(i)), visit)
			}
			return
		}
	}
	visit(root, base)
}

func appendKeys(path Path, keys []string) Path {
	out := append(Path{}, path...)
	for _, key := range keys {
		out = append(out, keyStep(key))
	}
	return out
}

func collectSupportedImages(docs []*yaml.Node) []ImageOccurrence {
	var occurrences []ImageOccurrence
	for docIndex, doc := range docs {
		base := Path{indexStep(docIndex)}
		iterResourceDocs(doc, base, func(resource *yaml.Node, resourcePath Path) {
			kind := scalarString(mapValue(resource, "kind"))
			podSpecRel, ok := supportedPodSpecPaths[kind]
			if !ok {
				return
			}
			podSpecPath := appendKeys(resourcePath, podSpecRel)
			podSpec, err := nodeAt(docs, podSpecPath)
			if err != nil || podSpec == nil || podSpec.Kind != yaml.MappingNode {
				return
			}
			for _, field := range containerFields {
				containers := mapValue(podSpec, field)
				if containers == nil || containers.Kind != yaml.SequenceNode {
					continue
				}
				for i, container := range containers.Content {
					if container.Kind != yaml.MappingNode {
						continue
					}
					imageNode := mapValue(container, "image")
					if imageNode == nil || imageNode.Kind != yaml.ScalarNode || imageNode.Value == "" {
						continue
					}
					imagePath := append(append(Path{}, podSpecPath...), keyStep(field), indexStep(i), keyStep("image"))
					occurrences = append(occurrences, ImageOccurrence{
						SourceImage: imageNode.Value,
						Path:        imagePath,
						Origin:      fmt.Sprintf("%s %s", resourceLabel(resource), imagePath.String()),
						Hook:        isHelmHook(resource),
					})
				}
			}
		})
	}
	return occurrences
}

func looksLikeImage(value string) bool {
	if strings.Contains(value, "://") || strings.Contains(value, " ") || strings.HasPrefix(value, "$") {
		return false
	}
	return imageLikeRE.MatchString(value) && (strings.Contains(value, "/") || strings.Contains(value, ":") || strings.Contains(value, "@sha256:"))
}

func collectUnsupportedImageFields(docs []*yaml.Node, supported map[string]bool) []UnsupportedField {
	var out []UnsupportedField
	var walk func(*yaml.Node, Path, string)
	walk = func(n *yaml.Node, path Path, keyHint string) {
		if n == nil {
			return
		}
		switch n.Kind {
		case yaml.DocumentNode:
			if len(n.Content) > 0 {
				walk(n.Content[0], path, keyHint)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				key := n.Content[i].Value
				walk(n.Content[i+1], append(append(Path{}, path...), keyStep(key)), key)
			}
		case yaml.SequenceNode:
			for i, child := range n.Content {
				walk(child, append(append(Path{}, path...), indexStep(i)), keyHint)
			}
		case yaml.ScalarNode:
			if supported[path.String()] {
				return
			}
			lower := strings.ToLower(keyHint)
			if (lower == "image" || strings.HasSuffix(lower, "image") || strings.Contains(lower, "image")) && looksLikeImage(n.Value) {
				out = append(out, UnsupportedField{Path: path.String(), Value: n.Value})
			}
		}
	}
	for i, doc := range docs {
		walk(doc, Path{indexStep(i)}, "")
	}
	return out
}

type imageRef struct {
	Registry string
	Path     string
	Tag      string
	Digest   string
}

func parseImageReference(ref string) imageRef {
	base := ref
	digest := ""
	if idx := strings.LastIndex(ref, "@"); idx >= 0 {
		base = ref[:idx]
		digest = ref[idx+1:]
	}
	tag := ""
	name := base
	lastSlash := strings.LastIndex(base, "/")
	lastColon := strings.LastIndex(base, ":")
	if lastColon > lastSlash {
		tag = base[lastColon+1:]
		name = base[:lastColon]
	}
	parts := strings.Split(name, "/")
	var registry, repoPath string
	switch {
	case len(parts) == 1:
		registry = "docker.io"
		repoPath = "library/" + parts[0]
	case strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") || parts[0] == "localhost":
		registry = parts[0]
		repoPath = strings.Join(parts[1:], "/")
	default:
		registry = "docker.io"
		repoPath = strings.Join(parts, "/")
	}
	if tag == "" && digest == "" {
		tag = "latest"
	}
	return imageRef{Registry: registry, Path: repoPath, Tag: tag, Digest: digest}
}

func sanitizeRepoPath(path string) string {
	path = strings.ToLower(path)
	re := regexp.MustCompile(`[^a-z0-9._/-]+`)
	return strings.Trim(re.ReplaceAllString(path, "-"), "/-")
}

func targetImageFor(sourceImage, targetRegistry string) string {
	parsed := parseImageReference(sourceImage)
	targetRegistry = strings.TrimRight(targetRegistry, "/")
	repo := sanitizeRepoPath(parsed.Registry + "/" + parsed.Path)
	tag := parsed.Tag
	if tag == "" {
		tag = strings.ReplaceAll(parsed.Digest, ":", "-")
		if len(tag) > 24 {
			tag = tag[:24]
		}
	}
	tag = regexp.MustCompile(`[^A-Za-z0-9_.-]+`).ReplaceAllString(tag, "-")
	return fmt.Sprintf("%s/%s:%s", targetRegistry, repo, tag)
}

func archiveTagFor(sourceImage, platform string) string {
	sum := sha256.Sum256([]byte(sourceImage + "|" + platform))
	return fmt.Sprintf("img-%x", sum[:8])
}

func addReason(reasons []string, reason string) []string {
	for _, existing := range reasons {
		if existing == reason {
			return reasons
		}
	}
	return append(reasons, reason)
}

func makeLock(occurrences []ImageOccurrence, targetRegistry, platform string, helmMajor int, unsupported []UnsupportedField) ImageLock {
	grouped := map[string]*ImageEntry{}
	for _, occurrence := range occurrences {
		entry, ok := grouped[occurrence.SourceImage]
		if !ok {
			parsed := parseImageReference(occurrence.SourceImage)
			entry = &ImageEntry{
				SourceImage:  occurrence.SourceImage,
				SourceDigest: parsed.Digest,
				TargetImage:  targetImageFor(occurrence.SourceImage, targetRegistry),
				Platform:     platform,
				ArchiveTag:   archiveTagFor(occurrence.SourceImage, platform),
				ProofStatus:  StatusUnproven,
			}
			grouped[occurrence.SourceImage] = entry
		}
		entry.Origins = append(entry.Origins, ImageOrigin{
			Path:     occurrence.Path.String(),
			Resource: occurrence.Origin,
			Hook:     occurrence.Hook,
		})
		if helmMajor <= 3 && occurrence.Hook {
			entry.Reasons = addReason(entry.Reasons, "helm3_hook_unproven")
		}
	}

	keys := make([]string, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	images := make([]ImageEntry, 0, len(keys))
	for _, key := range keys {
		entry := grouped[key]
		if entry.TargetDigest == "" {
			entry.Reasons = addReason(entry.Reasons, "target_digest_missing")
		}
		if len(unsupported) > 0 {
			entry.Reasons = addReason(entry.Reasons, "unsupported_image_fields_present")
		}
		images = append(images, *entry)
	}

	status := StatusProven
	if len(images) > 0 || len(unsupported) > 0 {
		status = StatusUnproven
	}
	return ImageLock{
		APIVersion:        "helm-capsule/v1alpha1",
		Kind:              "ImageLock",
		Status:            status,
		Images:            images,
		UnsupportedFields: unsupported,
		HelmMajor:         helmMajor,
	}
}

func targetRef(entry ImageEntry, requireDigest bool) (string, error) {
	if requireDigest && entry.TargetDigest == "" {
		return "", fmt.Errorf("missing target_digest for %s", entry.SourceImage)
	}
	if entry.TargetDigest != "" {
		return entry.TargetImage + "@" + entry.TargetDigest, nil
	}
	return entry.TargetImage, nil
}

func applyLockToDocs(docs []*yaml.Node, lock ImageLock, requireDigest bool) ([]*yaml.Node, []string) {
	rewritten := cloneDocs(docs)
	imageMap := map[string]string{}
	var errorsOut []string
	for _, entry := range lock.Images {
		target, err := targetRef(entry, requireDigest)
		if err != nil {
			errorsOut = append(errorsOut, err.Error())
			continue
		}
		imageMap[entry.SourceImage] = target
	}
	for _, occurrence := range collectSupportedImages(rewritten) {
		target, ok := imageMap[occurrence.SourceImage]
		if !ok {
			errorsOut = append(errorsOut, fmt.Sprintf("unlocked image at %s: %s", occurrence.Origin, occurrence.SourceImage))
			continue
		}
		node, err := nodeAt(rewritten, occurrence.Path)
		if err != nil {
			errorsOut = append(errorsOut, err.Error())
			continue
		}
		node.Kind = yaml.ScalarNode
		node.Tag = "!!str"
		node.Value = target
	}
	return rewritten, errorsOut
}

func nodeEqualExceptImagePaths(left, right *yaml.Node, path Path, imagePaths map[string]bool, changes *[]string) {
	if imagePaths[path.String()] {
		return
	}
	if left == nil || right == nil {
		if left != right {
			*changes = append(*changes, path.String())
		}
		return
	}
	if left.Kind != right.Kind || left.Tag != right.Tag || left.Value != right.Value || len(left.Content) != len(right.Content) {
		*changes = append(*changes, path.String())
		return
	}
	for i := range left.Content {
		childPath := path
		if left.Kind == yaml.MappingNode {
			if i%2 == 0 {
				continue
			}
			childPath = append(append(Path{}, path...), keyStep(left.Content[i-1].Value))
		} else if left.Kind == yaml.SequenceNode {
			childPath = append(append(Path{}, path...), indexStep(i))
		}
		nodeEqualExceptImagePaths(left.Content[i], right.Content[i], childPath, imagePaths, changes)
	}
}

func diffNonImageChanges(original, rewritten []*yaml.Node, imagePaths map[string]bool) []string {
	var changes []string
	if len(original) != len(rewritten) {
		return []string{"$"}
	}
	for i := range original {
		nodeEqualExceptImagePaths(documentRoot(original[i]), documentRoot(rewritten[i]), Path{indexStep(i)}, imagePaths, &changes)
	}
	return changes
}

func computeProof(rendered []byte, lock ImageLock, requireDigests bool) Proof {
	docs, err := parseYAMLDocuments(rendered)
	if err != nil {
		return Proof{Status: StatusFailed, Reason: "parse_or_proof_error", Error: err.Error()}
	}
	occurrences := collectSupportedImages(docs)
	imagePaths := map[string]bool{}
	for _, occurrence := range occurrences {
		imagePaths[occurrence.Path.String()] = true
	}
	unsupported := collectUnsupportedImageFields(docs, imagePaths)

	var missingDigests []string
	for _, entry := range lock.Images {
		if entry.TargetDigest == "" {
			missingDigests = append(missingDigests, entry.SourceImage)
		}
	}
	if requireDigests && len(missingDigests) > 0 {
		return Proof{
			Status:               StatusUnproven,
			Reason:               "target_digest_missing",
			ImageCount:           len(occurrences),
			MissingTargetDigests: missingDigests,
			UnsupportedFields:    unsupported,
		}
	}

	rewritten, applyErrors := applyLockToDocs(docs, lock, requireDigests)
	if len(applyErrors) > 0 {
		return Proof{
			Status:            StatusFailed,
			Reason:            "unlocked_images_found",
			ImageCount:        len(occurrences),
			Errors:            applyErrors,
			UnsupportedFields: unsupported,
		}
	}
	changes := diffNonImageChanges(docs, rewritten, imagePaths)
	if len(changes) > 0 {
		return Proof{
			Status:            StatusFailed,
			Reason:            "non_image_fields_changed",
			ImageCount:        len(occurrences),
			NonImageChanges:   changes,
			UnsupportedFields: unsupported,
		}
	}
	if len(unsupported) > 0 {
		return Proof{
			Status:            StatusUnproven,
			Reason:            "unsupported_image_fields_present",
			ImageCount:        len(occurrences),
			UnsupportedFields: unsupported,
		}
	}
	var hookImages []string
	for _, entry := range lock.Images {
		for _, reason := range entry.Reasons {
			if reason == "helm3_hook_unproven" {
				hookImages = append(hookImages, entry.SourceImage)
				break
			}
		}
	}
	if len(hookImages) > 0 {
		return Proof{Status: StatusUnproven, Reason: "helm3_hook_unproven", ImageCount: len(occurrences), Images: hookImages}
	}
	return Proof{Status: StatusProven, Reason: "image_fields_only", ImageCount: len(occurrences)}
}

func readLock(path string) (ImageLock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ImageLock{}, err
	}
	var lock ImageLock
	if strings.HasSuffix(path, ".json") {
		err = json.Unmarshal(data, &lock)
	} else {
		err = yaml.Unmarshal(data, &lock)
	}
	return lock, err
}

func writeLock(lock ImageLock, path string) error {
	jsonPath, yamlPath := lockOutputPaths(path)
	jsonData, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(jsonPath, append(jsonData, '\n'), 0644); err != nil {
		return err
	}
	yamlData, err := yaml.Marshal(lock)
	if err != nil {
		return err
	}
	return os.WriteFile(yamlPath, yamlData, 0644)
}

func lockOutputPaths(path string) (string, string) {
	switch {
	case strings.HasSuffix(path, ".json"):
		return path, strings.TrimSuffix(path, ".json") + ".yaml"
	case strings.HasSuffix(path, ".yaml"):
		return strings.TrimSuffix(path, ".yaml") + ".json", path
	case strings.HasSuffix(path, ".yml"):
		return strings.TrimSuffix(path, ".yml") + ".json", path
	default:
		return path + ".json", path + ".yaml"
	}
}

func renderChart(chart, release, namespace, kubeVersion, helmBinary string, values, apiVersions []string, renderedManifest string) ([]byte, error) {
	if renderedManifest != "" {
		return os.ReadFile(renderedManifest)
	}
	if chart == "" {
		return nil, errors.New("chart is required unless --rendered-manifest is set")
	}
	if _, err := exec.LookPath(helmBinary); err != nil {
		return nil, fmt.Errorf("helm binary not found: %s; install Helm or pass --rendered-manifest", helmBinary)
	}
	args := []string{"template", release, chart, "--namespace", namespace, "--include-crds"}
	if kubeVersion != "" {
		args = append(args, "--kube-version", kubeVersion)
	}
	for _, apiVersion := range apiVersions {
		args = append(args, "--api-versions", apiVersion)
	}
	for _, valueFile := range values {
		args = append(args, "-f", valueFile)
	}
	cmd := exec.Command(helmBinary, args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("helm template failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

func writePostRenderers(outDir string) error {
	shellScript := `#!/usr/bin/env sh
set -eu
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec helm-capsule post-render --lock "$DIR/images.lock.yaml"
`
	cmdScript := `@echo off
helm-capsule post-render --lock "%~dp0images.lock.yaml"
`
	shellPath := filepath.Join(outDir, "post-renderer")
	if err := os.WriteFile(shellPath, []byte(shellScript), 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "post-renderer.cmd"), []byte(cmdScript), 0644)
}

func copyValues(outDir string, values []string) error {
	if len(values) == 0 {
		return nil
	}
	valuesDir := filepath.Join(outDir, "values")
	if err := os.MkdirAll(valuesDir, 0755); err != nil {
		return err
	}
	for _, valueFile := range values {
		data, err := os.ReadFile(valueFile)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(valuesDir, filepath.Base(valueFile)), data, 0644); err != nil {
			return err
		}
	}
	return nil
}

func commandBuild(args []string) int {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	var values stringList
	var apiVersions stringList
	release := fs.String("release", "", "Helm release name")
	namespace := fs.String("namespace", "", "target namespace")
	targetRegistry := fs.String("target-registry", "", "internal registry prefix")
	platform := fs.String("platform", "linux/amd64", "target platform")
	kubeVersion := fs.String("kube-version", "", "Kubernetes version for helm template")
	outDir := fs.String("out", "", "capsule output directory")
	renderedManifest := fs.String("rendered-manifest", "", "pre-rendered manifest path")
	helmBinary := fs.String("helm-binary", "helm", "helm binary")
	helmMajor := fs.Int("helm-major", 4, "Helm major version proof mode")
	fs.Var(&values, "f", "values file")
	fs.Var(&values, "values", "values file")
	fs.Var(&apiVersions, "api-version", "Kubernetes API version for helm template")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	chart := ""
	if fs.NArg() > 0 {
		chart = fs.Arg(0)
	}
	if *release == "" || *namespace == "" || *targetRegistry == "" || *outDir == "" {
		return fail(errors.New("--release, --namespace, --target-registry, and --out are required"))
	}
	rendered, err := renderChart(chart, *release, *namespace, *kubeVersion, *helmBinary, values, apiVersions, *renderedManifest)
	if err != nil {
		return fail(err)
	}
	docs, err := parseYAMLDocuments(rendered)
	if err != nil {
		return fail(err)
	}
	occurrences := collectSupportedImages(docs)
	supported := map[string]bool{}
	for _, occurrence := range occurrences {
		supported[occurrence.Path.String()] = true
	}
	unsupported := collectUnsupportedImageFields(docs, supported)
	lock := makeLock(occurrences, *targetRegistry, *platform, *helmMajor, unsupported)
	if err := os.MkdirAll(*outDir, 0755); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(*outDir, "rendered.original.yaml"), rendered, 0644); err != nil {
		return fail(err)
	}
	if err := writeLock(lock, filepath.Join(*outDir, "images.lock.json")); err != nil {
		return fail(err)
	}
	proof := computeProof(rendered, lock, true)
	proofData, _ := json.MarshalIndent(proof, "", "  ")
	if err := os.WriteFile(filepath.Join(*outDir, "proof.json"), append(proofData, '\n'), 0644); err != nil {
		return fail(err)
	}
	if err := writePostRenderers(*outDir); err != nil {
		return fail(err)
	}
	if err := copyValues(*outDir, values); err != nil {
		return fail(err)
	}
	fmt.Println(proof.Status)
	fmt.Printf("images: %d\n", len(lock.Images))
	if proof.Status != StatusProven {
		fmt.Printf("reason: %s\n", proof.Reason)
	}
	if proof.Status == StatusFailed {
		return 1
	}
	return 0
}

func runChecked(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%s failed: %s", name, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func platformParts(platform string) (string, string) {
	parts := strings.Split(platform, "/")
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func mirrorWithCrane(entry ImageEntry) (string, error) {
	args := []string{"copy"}
	if entry.Platform != "" {
		args = append(args, "--platform", entry.Platform)
	}
	args = append(args, entry.SourceImage, entry.TargetImage)
	if _, err := runChecked("crane", args...); err != nil {
		return "", err
	}
	return runChecked("crane", "digest", entry.TargetImage)
}

func mirrorWithSkopeo(entry ImageEntry, ociLayout string) (string, error) {
	source := "docker://" + entry.SourceImage
	if ociLayout != "" {
		source = fmt.Sprintf("oci:%s:%s", ociLayout, entry.ArchiveTag)
	}
	args := []string{"copy"}
	if entry.Platform != "" {
		osName, arch := platformParts(entry.Platform)
		args = append(args, "--override-os", osName)
		if arch != "" {
			args = append(args, "--override-arch", arch)
		}
	}
	args = append(args, source, "docker://"+entry.TargetImage)
	if _, err := runChecked("skopeo", args...); err != nil {
		return "", err
	}
	return runChecked("skopeo", "inspect", "--format", "{{.Digest}}", "docker://"+entry.TargetImage)
}

func exportOCILayout(lock ImageLock, layoutDir string) error {
	if _, err := exec.LookPath("skopeo"); err != nil {
		return errors.New("skopeo is required for --oci-layout export")
	}
	if err := os.MkdirAll(layoutDir, 0755); err != nil {
		return err
	}
	for _, entry := range lock.Images {
		args := []string{"copy"}
		if entry.Platform != "" {
			osName, arch := platformParts(entry.Platform)
			args = append(args, "--override-os", osName)
			if arch != "" {
				args = append(args, "--override-arch", arch)
			}
		}
		args = append(args, "docker://"+entry.SourceImage, fmt.Sprintf("oci:%s:%s", layoutDir, entry.ArchiveTag))
		if _, err := runChecked("skopeo", args...); err != nil {
			return err
		}
	}
	return nil
}

func commandMirror(args []string) int {
	fs := flag.NewFlagSet("mirror", flag.ExitOnError)
	tool := fs.String("tool", "auto", "auto, crane, or skopeo")
	dryRun := fs.Bool("dry-run", false, "print mirror plan only")
	ociLayout := fs.String("oci-layout", "", "export image content to OCI layout using skopeo")
	push := fs.Bool("push", false, "push images even when --oci-layout is used")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if fs.NArg() != 1 {
		return fail(errors.New("mirror requires images.lock.yaml or images.lock.json"))
	}
	lockPath := fs.Arg(0)
	lock, err := readLock(lockPath)
	if err != nil {
		return fail(err)
	}
	if *dryRun {
		for _, entry := range lock.Images {
			fmt.Printf("%s -> %s\n", entry.SourceImage, entry.TargetImage)
		}
		return 0
	}
	if *ociLayout != "" {
		if err := exportOCILayout(lock, *ociLayout); err != nil {
			return fail(err)
		}
		if !*push {
			fmt.Println(StatusUnproven)
			fmt.Printf("oci-layout: %s\n", *ociLayout)
			return 0
		}
	}
	selectedTool := *tool
	if selectedTool == "auto" {
		if _, err := exec.LookPath("crane"); err == nil {
			selectedTool = "crane"
		} else if _, err := exec.LookPath("skopeo"); err == nil {
			selectedTool = "skopeo"
		} else {
			return fail(errors.New("no mirror tool found; install crane or skopeo"))
		}
	}
	for i := range lock.Images {
		var digest string
		var err error
		switch selectedTool {
		case "crane":
			digest, err = mirrorWithCrane(lock.Images[i])
		case "skopeo":
			digest, err = mirrorWithSkopeo(lock.Images[i], "")
		default:
			err = fmt.Errorf("unsupported mirror tool: %s", selectedTool)
		}
		if err != nil {
			return fail(err)
		}
		lock.Images[i].TargetDigest = digest
		lock.Images[i].ProofStatus = StatusProven
		lock.Images[i].Reasons = removeReason(lock.Images[i].Reasons, "target_digest_missing")
	}
	lock.Status = StatusProven
	for _, entry := range lock.Images {
		if entry.TargetDigest == "" {
			lock.Status = StatusUnproven
		}
	}
	if err := writeLock(lock, lockPath); err != nil {
		return fail(err)
	}
	fmt.Println(lock.Status)
	return 0
}

func removeReason(reasons []string, reason string) []string {
	out := reasons[:0]
	for _, r := range reasons {
		if r != reason {
			out = append(out, r)
		}
	}
	return out
}

func commandVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if fs.NArg() != 1 {
		return fail(errors.New("verify requires capsule directory"))
	}
	capsuleDir := fs.Arg(0)
	lock, err := readLock(filepath.Join(capsuleDir, "images.lock.json"))
	if err != nil {
		return fail(err)
	}
	rendered, err := os.ReadFile(filepath.Join(capsuleDir, "rendered.original.yaml"))
	if err != nil {
		return fail(err)
	}
	proof := computeProof(rendered, lock, true)
	proofData, _ := json.MarshalIndent(proof, "", "  ")
	if err := os.WriteFile(filepath.Join(capsuleDir, "proof.json"), append(proofData, '\n'), 0644); err != nil {
		return fail(err)
	}
	fmt.Println(proof.Status)
	if proof.Status != StatusProven {
		fmt.Printf("reason: %s\n", proof.Reason)
		return 1
	}
	return 0
}

func commandPostRender(args []string) int {
	fs := flag.NewFlagSet("post-render", flag.ExitOnError)
	lockPath := fs.String("lock", "", "images lock path")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if *lockPath == "" {
		return fail(errors.New("--lock is required"))
	}
	lock, err := readLock(*lockPath)
	if err != nil {
		return fail(err)
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fail(err)
	}
	docs, err := parseYAMLDocuments(input)
	if err != nil {
		return fail(err)
	}
	occurrences := collectSupportedImages(docs)
	supported := map[string]bool{}
	for _, occurrence := range occurrences {
		supported[occurrence.Path.String()] = true
	}
	unsupported := collectUnsupportedImageFields(docs, supported)
	if len(unsupported) > 0 {
		return fail(fmt.Errorf("unsupported image fields present: %+v", unsupported))
	}
	rewritten, applyErrors := applyLockToDocs(docs, lock, true)
	if len(applyErrors) > 0 {
		return fail(errors.New(strings.Join(applyErrors, "; ")))
	}
	out, err := encodeYAMLDocuments(rewritten)
	if err != nil {
		return fail(err)
	}
	_, _ = os.Stdout.Write(out)
	return 0
}

func commandExport(args []string) int {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	output := fs.String("output", "", "capsule archive output")
	metadataOnly := fs.Bool("metadata-only", false, "allow archive without OCI image layout")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if fs.NArg() != 1 || *output == "" {
		return fail(errors.New("export requires capsule directory and --output"))
	}
	capsuleDir := fs.Arg(0)
	lock, err := readLock(filepath.Join(capsuleDir, "images.lock.json"))
	if err != nil {
		return fail(err)
	}
	if len(lock.Images) > 0 && !*metadataOnly {
		if _, err := os.Stat(filepath.Join(capsuleDir, "oci-layout")); err != nil {
			return fail(errors.New("capsule has images but no oci-layout; run mirror --oci-layout or pass --metadata-only"))
		}
	}
	if strings.HasSuffix(*output, ".zst") {
		if _, err := exec.LookPath("tar"); err != nil {
			return fail(errors.New("tar is required to create .tar.zst archives"))
		}
		if _, err := runChecked("tar", "--zstd", "-cf", *output, "-C", capsuleDir, "."); err != nil {
			return fail(err)
		}
	} else {
		if err := createTar(capsuleDir, *output); err != nil {
			return fail(err)
		}
	}
	fmt.Println(StatusProven)
	fmt.Println(*output)
	return 0
}

func createTar(srcDir, output string) error {
	file, err := os.Create(output)
	if err != nil {
		return err
	}
	defer file.Close()
	tw := tar.NewWriter(file)
	defer tw.Close()
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil || rel == "." {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

func extractArchive(archive, outDir string) error {
	if strings.HasSuffix(archive, ".zst") {
		_, err := runChecked("tar", "-xf", archive, "-C", outDir)
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(outDir, filepath.Clean(header.Name))
		if !strings.HasPrefix(target, filepath.Clean(outDir)+string(os.PathSeparator)) && filepath.Clean(target) != filepath.Clean(outDir) {
			return fmt.Errorf("unsafe tar path: %s", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(file, tr); err != nil {
				_ = file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
		}
	}
}

func retargetLock(lock *ImageLock, targetRegistry string) {
	for i := range lock.Images {
		lock.Images[i].TargetImage = targetImageFor(lock.Images[i].SourceImage, targetRegistry)
		lock.Images[i].TargetDigest = ""
		lock.Images[i].ProofStatus = StatusUnproven
		lock.Images[i].Reasons = addReason(lock.Images[i].Reasons, "target_digest_missing")
	}
	lock.Status = StatusUnproven
}

func copyTree(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("output directory already exists: %s", dst)
	}
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}

func commandImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	targetRegistry := fs.String("target-registry", "", "internal registry prefix")
	outDir := fs.String("out", "imported-capsule", "output directory")
	dryRun := fs.Bool("dry-run", false, "extract and retarget only")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if fs.NArg() != 1 || *targetRegistry == "" {
		return fail(errors.New("import requires archive and --target-registry"))
	}
	tempDir, err := os.MkdirTemp("", "helm-capsule-import-*")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(tempDir)
	if err := extractArchive(fs.Arg(0), tempDir); err != nil {
		return fail(err)
	}
	lockPath := filepath.Join(tempDir, "images.lock.json")
	lock, err := readLock(lockPath)
	if err != nil {
		return fail(err)
	}
	retargetLock(&lock, *targetRegistry)
	layoutDir := filepath.Join(tempDir, "oci-layout")
	if len(lock.Images) > 0 {
		if _, err := os.Stat(layoutDir); err != nil {
			return fail(errors.New("archive does not contain oci-layout; disconnected import cannot mirror images"))
		}
	}
	if !*dryRun {
		if _, err := exec.LookPath("skopeo"); err != nil {
			return fail(errors.New("skopeo is required for disconnected import"))
		}
		for i := range lock.Images {
			digest, err := mirrorWithSkopeo(lock.Images[i], layoutDir)
			if err != nil {
				return fail(err)
			}
			lock.Images[i].TargetDigest = digest
			lock.Images[i].ProofStatus = StatusProven
			lock.Images[i].Reasons = removeReason(lock.Images[i].Reasons, "target_digest_missing")
		}
		lock.Status = StatusProven
	}
	if err := writeLock(lock, lockPath); err != nil {
		return fail(err)
	}
	if err := copyTree(tempDir, *outDir); err != nil {
		return fail(err)
	}
	if *dryRun {
		fmt.Println(StatusUnproven)
	} else {
		fmt.Println(StatusProven)
	}
	fmt.Println(*outDir)
	return 0
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, StatusFailed)
	fmt.Fprintln(os.Stderr, err)
	return 1
}

func usage() {
	fmt.Fprintf(os.Stderr, `helm-capsule

Usage:
  helm-capsule build <chart> --release NAME --namespace NS --target-registry REG --out DIR [flags]
  helm-capsule mirror <images.lock.yaml|json> [--dry-run] [--tool auto|crane|skopeo]
  helm-capsule verify <capsule-dir>
  helm-capsule post-render --lock <images.lock.yaml|json>
  helm-capsule export <capsule-dir> --output capsule.tar.zst
  helm-capsule import <capsule.tar.zst> --target-registry REG [--out DIR]
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "build":
		code = commandBuild(os.Args[2:])
	case "mirror":
		code = commandMirror(os.Args[2:])
	case "verify":
		code = commandVerify(os.Args[2:])
	case "post-render":
		code = commandPostRender(os.Args[2:])
	case "export":
		code = commandExport(os.Args[2:])
	case "import":
		code = commandImport(os.Args[2:])
	default:
		usage()
		code = 2
	}
	os.Exit(code)
}
