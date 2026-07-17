-- P2-2: object-storage engines became first-class names (minio, seaweedfs) so
-- CP_S3_ENGINES can gate them like CP_DB_ENGINES gates database engines. P2-1
-- rows recorded the engine as the resource kind ('s3'); rename those to the
-- real engine name so s3Info and the reconciler resolve the MinIO definition by
-- name (S3EngineByName). New rows already store 'minio'/'seaweedfs'.
UPDATE s3_credentials SET engine = 'minio' WHERE engine = 's3';
