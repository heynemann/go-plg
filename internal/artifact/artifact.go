// Package artifact is the format of a .plg artifact, which `plg pack` writes and plg.OpenArtifact reads: a gzip
// stream of a tar archive holding provenance.json (the [adminjson.Provenance] record) and then every file of the
// plugin, under plugin/, in the order fs.WalkDir visits them. Nothing in it depends on when, where or by whom it
// was written: every entry is a regular file with the same time (the Unix epoch), mode and owner, and the gzip
// header names no file and no time, so the same files and provenance always give the same bytes.
package artifact

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/bytedance/sonic"

	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/memfs"
)

// The archive's layout.
const (
	// ProvenanceName is the provenance record's entry, the archive's first.
	ProvenanceName = "provenance.json"
	// FilesDir is the directory the plugin's files are under, each at its path in the plugin.
	FilesDir = "plugin/"
	// Format is the version of the layout and of the provenance record, which the record holds: it changes only
	// when an entry or a field is removed or changes meaning.
	Format = 1
	// MaxProvenanceSize bounds the provenance record, which holds plg validate's report of the plugin.
	MaxProvenanceSize = 4 << 20
	// FileMode is every entry's mode.
	FileMode = 0o644
)

// DigestPrefix starts every content digest.
const DigestPrefix = "sha256:"

// ErrProvenanceSize reports a provenance record over [MaxProvenanceSize] bytes.
var ErrProvenanceSize = fmt.Errorf("the provenance record is over %d bytes", MaxProvenanceSize)

// Write writes the artifact of the plugin's files with provenance, whose Format it sets, to out.
func Write(out io.Writer, files *memfs.FS, provenance *adminjson.Provenance) error {
	record := *provenance
	record.Format = Format

	data, err := sonic.ConfigStd.Marshal(&record)
	if err != nil {
		return fmt.Errorf("writing the provenance record: %w", err)
	}

	if len(data) > MaxProvenanceSize {
		return ErrProvenanceSize
	}

	compressed := gzip.NewWriter(out) // its header names no file and no time
	archive := tar.NewWriter(compressed)

	err = writeEntry(archive, ProvenanceName, data)

	for name, contents := range files.Files() {
		if err != nil {
			break
		}

		err = writeEntry(archive, FilesDir+name, contents)
	}

	if err == nil {
		err = archive.Close()
	}

	if err == nil {
		err = compressed.Close()
	}

	if err != nil {
		return fmt.Errorf("writing the artifact: %w", err)
	}

	return nil
}

// writeEntry writes the regular file name holding data, with what every entry shares.
func writeEntry(archive *tar.Writer, name string, data []byte) error {
	err := archive.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg, Name: name, Size: int64(len(data)), Mode: FileMode, ModTime: time.Unix(0, 0),
	})
	if err == nil {
		_, err = archive.Write(data)
	}

	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	return nil
}

// Digest returns the content digest of a plugin's files: "sha256:" and the hex SHA-256 over every file's path and
// the SHA-256 of its contents, in the order fs.WalkDir visits them. It is the version Register gives a plugin whose
// descriptor names none (computed over the snapshot it loads, which holds regular files only) and the digest an
// artifact's provenance records.
func Digest(files *memfs.FS) string {
	sum := sha256.New()
	buf := make([]byte, 0, binary.MaxVarintLen64+sha256.Size)

	for name, contents := range files.Files() {
		file := sha256.Sum256(contents)
		buf = binary.AppendVarint(buf[:0], int64(len(name)))
		buf = append(buf, name...)
		buf = append(buf, file[:]...)
		sum.Write(buf)
	}

	digest := make([]byte, 0, len(DigestPrefix)+hex.EncodedLen(sha256.Size))
	digest = append(digest, DigestPrefix...)

	return string(hex.AppendEncode(digest, sum.Sum(buf[:0])))
}
