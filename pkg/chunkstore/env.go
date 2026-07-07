package chunkstore

import "os"

// L1FromEnv builds the L1 backend from environment configuration:
// EMBERVM_L1_DIR selects a directory backend (tests, NFS-style shared
// storage), EMBERVM_L1_ENDPOINT + friends select S3 (production, MinIO in
// CI). Returns (nil, false, nil) when no L1 is configured — L1 is optional
// everywhere; pause write-through and cross-node restore simply require it.
func L1FromEnv() (Backend, bool, error) {
	if dir := os.Getenv(S3EnvPrefix + "DIR"); dir != "" {
		b, err := NewDir(dir)
		if err != nil {
			return nil, false, err
		}
		return b, true, nil
	}
	cfg, ok, err := S3FromEnv()
	if err != nil || !ok {
		return nil, ok, err
	}
	b, err := NewS3(cfg)
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}
