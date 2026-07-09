package chunkstore

import "os"

// L1FromEnv builds the warm (L1) backend from environment configuration:
// EMBERVM_L1_DIR selects a directory backend (tests, NFS-style shared
// storage), EMBERVM_L1_ENDPOINT + friends select S3 (production, MinIO in
// CI). Returns (nil, false, nil) when no L1 is configured — L1 is optional
// everywhere; pause write-through and cross-node restore simply require it.
func L1FromEnv() (ListingBackend, bool, error) {
	return backendFromEnv(S3EnvPrefix)
}

// ColdEnvPrefix names the cold-tier (L2) variables: EMBERVM_COLD_ENDPOINT /
// _BUCKET / _ACCESS_KEY / _SECRET_KEY / _PREFIX / _SECURE, or _DIR.
const ColdEnvPrefix = "EMBERVM_COLD_"

// ColdFromEnv builds the cold (L2) backend — same schema as L1FromEnv with
// the EMBERVM_COLD_ prefix. In CI both point at one MinIO with different
// buckets; in production this is B2/R2/Glacier-IR per docs/zh/04 §7.
func ColdFromEnv() (ListingBackend, bool, error) {
	return backendFromEnv(ColdEnvPrefix)
}

func backendFromEnv(prefix string) (ListingBackend, bool, error) {
	if dir := os.Getenv(prefix + "DIR"); dir != "" {
		// L1/cold is the write-through RPO target: writes must survive
		// power loss, so this Dir fsyncs (the node-local cache does not).
		b, err := NewDurableDir(dir)
		if err != nil {
			return nil, false, err
		}
		return b, true, nil
	}
	cfg, ok, err := s3ConfigFromEnv(prefix)
	if err != nil || !ok {
		return nil, ok, err
	}
	b, err := NewS3(cfg)
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}
