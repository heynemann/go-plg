package plg

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/bytedance/sonic"

	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/artifact"
	"github.com/heynemann/go-plg/internal/memfs"
)

// Provenance is what `plg pack` records of a plugin in its artifact: where it comes from and how it was checked.
// plg records it and shows it ([PluginState], [AdminHandler]); it enforces nothing at load.
type Provenance struct {
	// Contract is the contract module the plugin was validated against: the one its plugin.yaml pins (plg pack
	// refuses while a replace points it elsewhere).
	Contract ContractModule
	// Digest is the content digest of the plugin's files, "sha256:" and 64 hex digits: the version Register gives
	// the plugin when its plugin.yaml names none. [OpenArtifact] checks it against the files.
	Digest string
	// Commit is the git commit the plugin was packed from; empty when its directory is not in a git repository.
	Commit string
	// PlgVersion is the version of plg (its module) that packed it, "(devel)" for a plg built from a checkout.
	PlgVersion string
	// Report is the JSON of plg validate's report of the plugin as plg pack validated it (its schema: the README),
	// with that one plugin and without the resource report, whose measurements change from run to run.
	Report []byte
	// Dirty reports that the files packed (the plugin's, the author repository's shared directories and plg.yaml)
	// differed from Commit: changed, added, untracked or ignored files.
	Dirty bool
}

// ContractModule is a contract module and its version.
type ContractModule struct {
	// Path is the module path.
	Path string
	// Version is the module's version.
	Version string
}

// Artifact is a plugin packed by `plg pack` (a .plg file), opened by [OpenArtifact]: the plugin's files, as an
// fs.FS ([fs.ReadFileFS], [fs.ReadDirFS] and [fs.StatFS]) holding plugin.yaml and every file the plugin loads,
// the author repository's shared directories copied in, and its [Provenance]. It is immutable and safe for
// concurrent use.
//
// A storage keeping one artifact per plugin lists each with [ParseManifest] of its plugin.yaml and returns the
// *Artifact itself from Open: Register then loads its files as they are (no copy), and [Info] and the
// [AdminHandler] show its provenance. A storage returning anything else (a wrapper, a copy) loads the same files
// without the provenance.
type Artifact struct {
	files      *memfs.FS
	provenance Provenance
}

var (
	_ fs.ReadFileFS = (*Artifact)(nil)
	_ fs.ReadDirFS  = (*Artifact)(nil)
	_ fs.StatFS     = (*Artifact)(nil)
)

// Open opens the named file or directory of the plugin.
func (a *Artifact) Open(name string) (fs.File, error) {
	return a.files.Open(name) //nolint:wrapcheck // fs.FS's own errors
}

// ReadFile returns a copy of the named file's contents.
func (a *Artifact) ReadFile(name string) ([]byte, error) {
	return a.files.ReadFile(name) //nolint:wrapcheck // fs.FS's own errors
}

// ReadDir returns the named directory's entries, sorted by name.
func (a *Artifact) ReadDir(name string) ([]fs.DirEntry, error) {
	return a.files.ReadDir(name) //nolint:wrapcheck // fs.FS's own errors
}

// Stat describes the named file or directory.
func (a *Artifact) Stat(name string) (fs.FileInfo, error) {
	return a.files.Stat(name) //nolint:wrapcheck // fs.FS's own errors
}

// Provenance returns the artifact's provenance. Its Report is the artifact's own: treat it as read-only.
func (a *Artifact) Provenance() Provenance {
	return a.provenance
}

// ErrInvalidArtifact reports an archive that is not an artifact plg pack writes: not gzip-compressed tar, cut
// short or corrupted (a checksum or the content digest does not match), an entry that is no regular file or
// whose path is not inside the plugin, a file twice, no provenance record or plugin.yaml, or a format this plg
// does not read.
var ErrInvalidArtifact = errors.New("plg: invalid artifact")

// maxArtifactStream bounds what OpenArtifact decompresses, whatever the archive's entries say: the files' and the
// provenance record's limits plus, per entry, its header (and an extended header for a long path) and padding.
const maxArtifactStream = MaxPluginSize + artifact.MaxProvenanceSize + 2*MaxPluginPathBytes +
	(MaxPluginEntries+2)*artifactEntryOverhead

// artifactEntryOverhead is what an entry adds to a tar stream at most beside its path: two headers (the entry's
// and an extended one) and the padding of each and of the data, in 512-byte blocks.
const artifactEntryOverhead = 4 * 512

// OpenArtifact reads the artifact of size bytes at r, a .plg file written by `plg pack`, into memory, and returns
// its files and provenance. It holds the files to the limits Register holds a storage's files to: a file of more
// than [MaxPluginFileSize] bytes, files of more than [MaxPluginSize] bytes together, more than [MaxPluginEntries]
// entries or entry paths over [MaxPluginPathBytes] bytes together fail with [ErrPluginTooLarge], before they are
// read, and decompression stops past what those limits allow. Anything else that is not an artifact fails with
// [ErrInvalidArtifact]; so do files whose content digest is not the one the provenance records. Nothing about
// the provenance is enforced: a plugin loads whatever its report or commit says.
func OpenArtifact(r io.ReaderAt, size int64) (*Artifact, error) {
	art, err := readArtifact(io.NewSectionReader(r, 0, size))
	if err != nil {
		return nil, fmt.Errorf("plg: opening an artifact: %w", err)
	}

	return art, nil
}

// readArtifact reads the artifact compressed.
func readArtifact(compressed io.Reader) (*Artifact, error) {
	unzipped, err := gzip.NewReader(compressed)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidArtifact, err)
	}

	unzipped.Multistream(false)

	stream := &boundedReader{r: unzipped, left: maxArtifactStream}
	reading := artifactReader{archive: tar.NewReader(stream)}

	err = reading.readEntries()
	if err == nil {
		// The rest of the stream (only the gzip trailer once the archive ends) is read for its checksum.
		_, err = io.Copy(io.Discard, stream)
		err = invalid(err)
	}

	if err != nil {
		return nil, err
	}

	return reading.artifact()
}

// invalid makes err, from reading the archive, an ErrInvalidArtifact unless it is one or an ErrPluginTooLarge.
func invalid(err error) error {
	if err == nil || errors.Is(err, ErrInvalidArtifact) || errors.Is(err, ErrPluginTooLarge) {
		return err
	}

	return fmt.Errorf("%w: %w", ErrInvalidArtifact, err)
}

// boundedReader reads r, failing with ErrPluginTooLarge past left bytes.
type boundedReader struct {
	r    io.Reader
	left int64
}

func (b *boundedReader) Read(buf []byte) (int, error) {
	if b.left <= 0 {
		return 0, fmt.Errorf("%w: the artifact decompresses to more than %d bytes", ErrPluginTooLarge,
			maxArtifactStream)
	}

	if int64(len(buf)) > b.left {
		buf = buf[:b.left]
	}

	n, err := b.r.Read(buf)
	b.left -= int64(n)

	return n, err //nolint:wrapcheck // readArtifact wraps it
}

// artifactReader reads an artifact's entries, charging the plugin's files to the limits of a plugin's files.
type artifactReader struct {
	archive    *tar.Reader
	tree       memfs.Builder
	provenance []byte // nil until the record is read
	count      int    // the plugin's files so far
	pathBytes  int
	total      int64
	manifest   bool // plugin.yaml was read
}

// readEntries reads every entry of the archive.
func (a *artifactReader) readEntries() error {
	for {
		header, err := a.archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err == nil {
			err = a.entry(header)
		}

		if err != nil {
			return invalid(err)
		}
	}
}

// entry reads the entry header describes: the provenance record, or a regular file of the plugin.
func (a *artifactReader) entry(header *tar.Header) error {
	name, isFile := strings.CutPrefix(header.Name, artifact.FilesDir)

	switch {
	case header.Name == artifact.ProvenanceName:
		return a.record(header)
	case !isFile || !fs.ValidPath(name) || name == ".":
		return fmt.Errorf("%w: the entry %q is not inside the plugin (%s)", ErrInvalidArtifact, header.Name,
			artifact.FilesDir)
	case header.Typeflag != tar.TypeReg:
		return fmt.Errorf("%w: %s is not a regular file", ErrInvalidArtifact, header.Name)
	default:
	}

	err := a.charge(name)
	if err != nil {
		return err
	}

	return a.file(name, header.Size)
}

// charge counts the entry name against [MaxPluginEntries] and its path against [MaxPluginPathBytes].
func (a *artifactReader) charge(name string) error {
	a.count++
	a.pathBytes += len(name)

	switch {
	case a.count > MaxPluginEntries:
		return fmt.Errorf("%w: %s brings the plugin over %d entries", ErrPluginTooLarge, name, MaxPluginEntries)
	case a.pathBytes > MaxPluginPathBytes:
		return fmt.Errorf("%w: %s brings the plugin's entry paths over %d bytes", ErrPluginTooLarge, name,
			MaxPluginPathBytes)
	default:
		return nil
	}
}

// file reads the regular file name, of size bytes.
func (a *artifactReader) file(name string, size int64) error {
	switch {
	case size > MaxPluginFileSize:
		return fileTooLarge(name)
	case a.total+size > MaxPluginSize:
		return pluginTooLarge(name)
	default:
	}

	a.total += size

	data, err := a.read(name, size)
	if err == nil {
		err = a.tree.Add(name, data)
	}

	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidArtifact, err)
	}

	a.manifest = a.manifest || name == manifestName

	return nil
}

// record reads the provenance record.
func (a *artifactReader) record(header *tar.Header) error {
	switch {
	case a.provenance != nil:
		return fmt.Errorf("%w: two %s entries", ErrInvalidArtifact, artifact.ProvenanceName)
	case header.Typeflag != tar.TypeReg:
		return fmt.Errorf("%w: %s is not a regular file", ErrInvalidArtifact, artifact.ProvenanceName)
	case header.Size > artifact.MaxProvenanceSize:
		return fmt.Errorf("%w: %w", ErrInvalidArtifact, artifact.ErrProvenanceSize)
	default:
	}

	data, err := a.read(artifact.ProvenanceName, header.Size)
	if err != nil {
		return err
	}

	a.provenance = data

	return nil
}

// read reads the entry name's size bytes.
func (a *artifactReader) read(name string, size int64) ([]byte, error) {
	data := make([]byte, size)

	_, err := io.ReadFull(a.archive, data)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}

	return data, nil
}

// artifact checks what the entries held and returns the artifact.
func (a *artifactReader) artifact() (*Artifact, error) {
	switch {
	case a.provenance == nil:
		return nil, fmt.Errorf("%w: no %s", ErrInvalidArtifact, artifact.ProvenanceName)
	case !a.manifest:
		return nil, fmt.Errorf("%w: no %s%s", ErrInvalidArtifact, artifact.FilesDir, manifestName)
	default:
	}

	var record adminjson.Provenance

	err := sonic.ConfigStd.Unmarshal(a.provenance, &record)

	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidArtifact, artifact.ProvenanceName, err)
	case record.Format != artifact.Format:
		return nil, fmt.Errorf("%w: format %d, but this plg reads format %d", ErrInvalidArtifact, record.Format,
			artifact.Format)
	default:
	}

	files, err := a.tree.Build()
	if err != nil {
		return nil, invalid(err)
	}

	if digest := artifact.Digest(files); digest != record.Digest {
		return nil, fmt.Errorf("%w: its files' digest is %s, but its provenance records %s", ErrInvalidArtifact,
			digest, record.Digest)
	}

	return &Artifact{files: files, provenance: newProvenance(&record)}, nil
}

// newProvenance returns the Provenance of an artifact's record.
func newProvenance(record *adminjson.Provenance) Provenance {
	return Provenance{
		Report:     []byte(record.Report),
		Contract:   ContractModule(record.Contract),
		Digest:     record.Digest,
		Commit:     record.Commit,
		PlgVersion: record.Plg,
		Dirty:      record.Dirty,
	}
}

// newAdminProvenance returns the admin JSON of src; nil for nil.
func newAdminProvenance(src *Provenance) *adminjson.Provenance {
	if src == nil {
		return nil
	}

	return &adminjson.Provenance{
		Report:   sonic.NoCopyRawMessage(src.Report),
		Contract: adminjson.Module(src.Contract),
		Digest:   src.Digest,
		Commit:   src.Commit,
		Plg:      src.PlgVersion,
		Dirty:    src.Dirty,
	}
}
