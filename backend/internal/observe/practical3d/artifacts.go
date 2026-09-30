package practical3d

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Q6FileManifestSchema is the frozen format of the artifact addressed by
// Q6_oracle.file_manifest_sha256: every reviewable file with its digest and
// line count, bound to review_target_sha256.
const Q6FileManifestSchema = "ao.q6.file-manifest.v1"

// Q6FileManifest is the frozen reviewable-file manifest.
type Q6FileManifest struct {
	Schema             string   `json:"schema"`
	ReviewTargetSHA256 string   `json:"review_target_sha256"`
	Files              []Q6File `json:"files"`
}

// Q6File is one reviewable file.
type Q6File struct {
	File       string `json:"file"`
	FileSHA256 string `json:"file_sha256"`
	LineCount  int    `json:"line_count"`
}

// preflightArtifacts verifies every content-addressed artifact the manifest
// names, copies each verified blob into <dst>/sha256/<digest> (read-only
// evidence covered by the canonical manifest digests), validates the Q6 file
// manifest against the mandatory defects, and returns per-task attachment
// spans used to prove OFF requests carry no Project Memory bytes.
func preflightArtifacts(m Manifest, r ArtifactResolver, dst string) (map[string][][]byte, error) {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrPrestartInvalid, fmt.Sprintf(format, args...))
	}
	blobs := map[string][]byte{}
	load := func(digest string) ([]byte, error) {
		if b, ok := blobs[digest]; ok {
			return b, nil
		}
		b, err := r.ReadDigest(digest)
		if err != nil {
			return nil, fail("content-addressed artifact %s: %v", digest, err)
		}
		if sha256Hex(b) != digest {
			return nil, fail("content-addressed artifact %s digest mismatch", digest)
		}
		blobs[digest] = b
		return b, nil
	}
	digests := make([]string, 0, 4+2*len(m.Tasks)+len(m.Q4Oracle.TaskOracles))
	digests = append(digests, m.Q1Oracle.VerifyCommandSHA256, m.Q4Oracle.CommandSHA256, m.Q6Oracle.FileManifestSHA256, m.Q6Oracle.ReviewTargetSHA256)
	for _, task := range m.Tasks {
		digests = append(digests, task.TaskManifestSHA256, task.OracleRef)
	}
	for _, task := range m.Q4Oracle.TaskOracles {
		digests = append(digests, task.HiddenTestManifestSHA256)
	}
	for _, d := range digests {
		if _, err := load(d); err != nil {
			return nil, err
		}
	}
	if err := validateQ6FileManifest(m.Q6Oracle, blobs[m.Q6Oracle.FileManifestSHA256], load); err != nil {
		return nil, err
	}
	spans := map[string][][]byte{}
	byRef := map[string]string{}
	for _, cell := range m.TreatmentMapping {
		a := cell.ASSISTED
		if a.AttachmentPresent == nil || !*a.AttachmentPresent {
			continue
		}
		if prev, ok := byRef[a.AttachmentArtifactRef]; ok && prev != a.AttachmentSHA256 {
			return nil, fail("treatment artifact ref %s has conflicting digests", a.AttachmentArtifactRef)
		}
		byRef[a.AttachmentArtifactRef] = a.AttachmentSHA256
		b, ok := blobs[a.AttachmentSHA256]
		if !ok {
			var err error
			b, err = r.ReadArtifact(a.AttachmentArtifactRef)
			if err != nil {
				return nil, fail("treatment artifact %s: %v", a.AttachmentArtifactRef, err)
			}
			if sha256Hex(b) != a.AttachmentSHA256 {
				return nil, fail("treatment artifact %s digest mismatch", a.AttachmentArtifactRef)
			}
			blobs[a.AttachmentSHA256] = b
		}
		spans[cell.TaskID] = append(spans[cell.TaskID], attachmentSpans(b)...)
	}
	if err := os.MkdirAll(filepath.Join(dst, "sha256"), 0o700); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(blobs))
	for d := range blobs {
		keys = append(keys, d)
	}
	sort.Strings(keys)
	for _, d := range keys {
		if err := writeExclusive(filepath.Join(dst, "sha256", d), blobs[d]); err != nil {
			return nil, err
		}
	}
	return spans, nil
}

func validateQ6FileManifest(q Q6Oracle, raw []byte, load func(string) ([]byte, error)) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: Q6 file manifest: %s", ErrPrestartInvalid, fmt.Sprintf(format, args...))
	}
	var fm Q6FileManifest
	if err := strictUnmarshal(raw, &fm); err != nil {
		return fail("%v", err)
	}
	if fm.Schema != Q6FileManifestSchema || fm.ReviewTargetSHA256 != q.ReviewTargetSHA256 || len(fm.Files) == 0 {
		return fail("schema/target mismatch")
	}
	files := map[string]Q6File{}
	for i, f := range fm.Files {
		if !safeRelative(f.File) || !validSHA256(f.FileSHA256) || f.LineCount <= 0 || (i > 0 && f.File <= fm.Files[i-1].File) {
			return fail("file rows must be safe, sorted and unique")
		}
		b, err := load(f.FileSHA256)
		if err != nil {
			return err
		}
		if lineCount(b) != f.LineCount {
			return fail("%s line count differs from its blob", f.File)
		}
		files[f.File] = f
	}
	for _, d := range q.MandatoryDefects {
		f, ok := files[d.File]
		if !ok || f.FileSHA256 != d.FileSHA256 || d.CausalLine > f.LineCount {
			return fail("defect %s file/digest/causal_line not within the review target", d.DefectID)
		}
	}
	return nil
}

func lineCount(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte{'\n'})
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

func invocationConfig(m Manifest, task string, role Role, class CallClass) (InvocationConfig, bool) {
	for _, x := range m.InvocationConfigs {
		if x.TaskID == task && x.Role == role && x.CallClass == class {
			return x, true
		}
	}
	return InvocationConfig{}, false
}

func roleDeadline(m Manifest, role Role) int64 {
	for _, x := range m.Deadlines.RoleSeconds {
		if x.Role == role {
			return x.Seconds
		}
	}
	return 0
}
