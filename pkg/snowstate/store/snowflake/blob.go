package snowflake

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	sf "github.com/snowflakedb/gosnowflake"

	"github.com/thunter/snowstate/pkg/snowstate/store"
)

var errStandardUnsupported = errors.New("snowflake: locking.mode=standard is not available")

// blobPathRE guards the paths spliced into PUT, GET and REMOVE. Keys and
// version IDs already fit it; this keeps a corrupt row from reaching SQL.
var blobPathRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

func checkBlobPath(p string) error {
	if !blobPathRE.MatchString(p) {
		return fmt.Errorf("snowflake: bad blob path %q", p)
	}
	return nil
}

func (s *Store) stage(key string) string { return "@" + s.dataTable(key, "STATE_BLOBS") }

// PutBlob implements store.Store. PUT with a stream takes the object name from
// the base name of the file URL.
func (s *Store) PutBlob(ctx context.Context, key, versionID string, gz []byte) (string, error) {
	p := key + "/" + versionID + ".json.gz"
	if err := checkBlobPath(p); err != nil {
		return "", err
	}
	stmt := fmt.Sprintf("PUT 'file:///snowstate/%s' '%s/%s' AUTO_COMPRESS=FALSE SOURCE_COMPRESSION=GZIP OVERWRITE=TRUE",
		path.Base(p), s.stage(key), path.Dir(p))
	err := retry(ctx, func() error {
		_, err := s.exec(sf.WithFileStream(ctx, bytes.NewReader(gz)), s.db, "blob_put", stmt)
		return err
	})
	if err != nil {
		return "", err
	}
	return p, nil
}

// GetBlob implements store.Store.
func (s *Store) GetBlob(ctx context.Context, key, p string) ([]byte, error) {
	if err := checkBlobPath(p); err != nil {
		return nil, err
	}
	var out []byte
	err := retry(ctx, func() error {
		var buf bytes.Buffer
		c := sf.WithFileTransferOptions(ctx, &sf.SnowflakeFileTransferOptions{GetFileToStream: true})
		c = sf.WithFileGetStream(c, &buf)
		stmt := fmt.Sprintf("GET '%s/%s' 'file:///tmp/'", s.stage(key), p)
		rows, err := s.query(c, s.db, "blob_get", stmt)
		if errorNumber(err) == codeNoFilesToGet {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		if err := rows.Err(); err != nil {
			return err
		}
		if buf.Len() == 0 {
			return store.ErrNotFound
		}
		out = buf.Bytes()
		return nil
	})
	return out, err
}

// RemoveBlob implements store.Store.
func (s *Store) RemoveBlob(ctx context.Context, key, p string) error {
	if err := checkBlobPath(p); err != nil {
		return err
	}
	_, err := s.exec(ctx, s.db, "blob_remove", fmt.Sprintf("REMOVE '%s/%s'", s.stage(key), p))
	return classify(err)
}

// ListBlobs implements store.Store.
func (s *Store) ListBlobs(ctx context.Context) ([]store.Blob, error) {
	schemas, err := s.dataSchemas(ctx)
	if err != nil {
		return nil, err
	}
	var out []store.Blob
	for _, schema := range schemas {
		rows, err := s.query(ctx, s.db, "blob_list", "LIST '@"+s.table(schema, "STATE_BLOBS")+"'")
		if err != nil {
			return nil, classify(err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var name, md5, modified string
				var size sql.NullInt64
				if err := rows.Scan(&name, &size, &md5, &modified); err != nil {
					return err
				}
				// LIST returns state_blobs/<key>/<version>.json.gz.
				_, rel, ok := strings.Cut(name, "/")
				if !ok {
					continue
				}
				at, _ := time.Parse(time.RFC1123, modified) // zero means "unknown age"
				out = append(out, store.Blob{Path: rel, Key: path.Dir(rel), LastModified: at})
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, classify(err)
		}
	}
	return out, nil
}
