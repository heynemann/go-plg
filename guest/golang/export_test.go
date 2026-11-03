package golang

// InflateArtifact exposes inflate.
func InflateArtifact(compressed []byte) ([]byte, error) { return inflate(compressed) }
