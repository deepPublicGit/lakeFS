package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-openapi/swag"
	"github.com/treeverse/lakefs/pkg/api/apigen"
	"github.com/treeverse/lakefs/pkg/api/helpers"
	"github.com/treeverse/lakefs/pkg/fileutil"
	"github.com/treeverse/lakefs/pkg/uri"
	"golang.org/x/sync/errgroup"
)

func getMtimeFromStats(stats apigen.ObjectStats) (int64, error) {
	if stats.Metadata == nil {
		return stats.Mtime, nil
	}
	clientMtime, hasClientMtime := stats.Metadata.Get(ClientMtimeMetadataKey)
	if hasClientMtime {
		// parse
		return strconv.ParseInt(clientMtime, 10, 64)
	}
	return stats.Mtime, nil
}

type Tasks struct {
	Downloaded uint64
	Uploaded   uint64
	Removed    uint64
}

type SyncManager struct {
	ctx         context.Context
	client      *apigen.ClientWithResponses
	httpClient  *http.Client
	progressBar *ProgressPool
	tasks       Tasks
	cfg         Config
}

func NewSyncManager(ctx context.Context, client *apigen.ClientWithResponses, httpClient *http.Client, cfg Config) *SyncManager {
	return &SyncManager{
		ctx:         ctx,
		client:      client,
		httpClient:  httpClient,
		progressBar: NewProgressPool(),
		cfg:         cfg,
	}
}

// Sync - sync changes between remote and local directory given the Changes channel.
// For each change, will apply download, upload or delete according to the change type and change source
func (s *SyncManager) Sync(rootPath string, remote *uri.URI, changeSet <-chan *Change) error {
	s.progressBar.Start()
	defer s.progressBar.Stop()

	wg, ctx := errgroup.WithContext(s.ctx)
	for i := 0; i < s.cfg.SyncFlags.Parallelism; i++ {
		wg.Go(func() error {
			for change := range changeSet {
				if err := s.apply(ctx, rootPath, remote, change); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err := wg.Wait(); err != nil {
		return err
	}
	if s.cfg.IncludePerm {
		return nil // Do not prune directories in this case to preserve directories and permissions
	}
	_, err := fileutil.PruneEmptyDirectories(rootPath)
	return err
}

func (s *SyncManager) apply(ctx context.Context, rootPath string, remote *uri.URI, change *Change) error {
	switch change.Type {
	case ChangeTypeAdded, ChangeTypeModified:
		switch change.Source {
		case ChangeSourceRemote:
			// remotely changed something, download it!
			if err := s.download(ctx, rootPath, remote, change.Path); err != nil {
				return fmt.Errorf("download %s failed: %w", change.Path, err)
			}
		case ChangeSourceLocal:
			// we wrote something, upload it!
			if err := s.upload(ctx, rootPath, remote, change.Path); err != nil {
				return fmt.Errorf("upload %s failed: %w", change.Path, err)
			}
		default:
			panic("invalid change source")
		}
	case ChangeTypeRemoved:
		if change.Source == ChangeSourceRemote {
			// remote deleted something, delete it locally!
			if err := s.deleteLocal(rootPath, change); err != nil {
				return fmt.Errorf("delete local %s failed: %w", change.Path, err)
			}
		} else {
			// we deleted something, delete it on remote!
			if err := s.deleteRemote(ctx, remote, change); err != nil {
				return fmt.Errorf("delete remote %s failed: %w", change.Path, err)
			}
		}
	case ChangeTypeConflict:
		return ErrConflict
	default:
		panic("invalid change type")
	}
	return nil
}

func (s *SyncManager) downloadFile(ctx context.Context, remote *uri.URI, path, destination string, objStat apigen.ObjectStats) error {
	sizeBytes := swag.Int64Value(objStat.SizeBytes)
	f, err := os.Create(destination)
	if err != nil {
		// Sometimes we get a file that is actually a directory marker (Spark loves writing those).
		// If we already have the directory, we can skip it.
		if errors.Is(err, syscall.EISDIR) && sizeBytes == 0 {
			return nil // no further action required!
		}
		return fmt.Errorf("could not create file '%s': %w", destination, err)
	}
	defer func() {
		err = f.Close()
	}()

	if sizeBytes == 0 { // if size is empty just create file
		spinner := s.progressBar.AddSpinner("download " + path)
		atomic.AddUint64(&s.tasks.Downloaded, 1)
		defer spinner.Done()
	} else {
		var body io.Reader
		if s.cfg.SyncFlags.Presign {
			resp, err := s.httpClient.Get(objStat.PhysicalAddress)
			if err != nil {
				return err
			}
			defer func() {
				_ = resp.Body.Close()
			}()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("%s (pre-signed GET: HTTP %d): %w", path, resp.StatusCode, ErrDownloadingFile)
			}
			body = resp.Body
		} else {
			resp, err := s.client.GetObject(ctx, remote.Repository, remote.Ref, &apigen.GetObjectParams{
				Path: filepath.ToSlash(filepath.Join(remote.GetPath(), path)),
			})
			if err != nil {
				return err
			}
			defer func() {
				_ = resp.Body.Close()
			}()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("%s (GetObject: HTTP %d): %w", path, resp.StatusCode, ErrDownloadingFile)
			}
			body = resp.Body
		}

		b := s.progressBar.AddReader(fmt.Sprintf("download %s", path), sizeBytes)
		barReader := b.Reader(body)
		defer func() {
			if err != nil {
				b.Error()
			} else {
				atomic.AddUint64(&s.tasks.Downloaded, 1)
				b.Done()
			}
		}()

		_, err = io.Copy(f, barReader)
		if err != nil {
			return fmt.Errorf("could not write file '%s': %w", destination, err)
		}
	}
	return nil
}

func (s *SyncManager) download(ctx context.Context, rootPath string, remote *uri.URI, path string) error {
	if err := fileutil.VerifyRelPath(strings.TrimPrefix(path, uri.PathSeparator), rootPath); err != nil {
		return err
	}
	destination := filepath.ToSlash(fmt.Sprintf("%s%s%s", rootPath, uri.PathSeparator, path))
	destinationDirectory := filepath.Dir(destination)

	if err := os.MkdirAll(destinationDirectory, os.FileMode(DefaultDirectoryPermissions)); err != nil {
		return err
	}
	statResp, err := s.client.StatObjectWithResponse(ctx, remote.Repository, remote.Ref, &apigen.StatObjectParams{
		Path:         filepath.ToSlash(filepath.Join(remote.GetPath(), path)),
		Presign:      swag.Bool(s.cfg.SyncFlags.Presign),
		UserMetadata: swag.Bool(true),
	})
	if err != nil {
		return err
	}
	if statResp.StatusCode() != http.StatusOK {
		httpErr := apigen.Error{Message: "no content"}
		_ = json.Unmarshal(statResp.Body, &httpErr)
		return fmt.Errorf("(stat: HTTP %d, message: %s): %w", statResp.StatusCode(), httpErr.Message, ErrDownloadingFile)
	}
	objStat := *statResp.JSON200
	// get mtime
	mtimeSecs, err := getMtimeFromStats(objStat)
	if err != nil {
		return err
	}
	lastModified := time.Unix(mtimeSecs, 0)

	var perm *POSIXPermissions
	isDir := strings.HasSuffix(path, uri.PathSeparator)
	if s.cfg.IncludePerm { // Optimization - fail on to get permissions from metadata before having to download the entire file
		if perm, err = getPermissionFromStats(objStat, true); err != nil {
			return err
		}
	} else if isDir {
		// Directory marker - skip
		return nil
	}

	if !isDir {
		if err = s.downloadFile(ctx, remote, path, destination, objStat); err != nil {
			return err
		}
	}
	// set mtime to the server returned one
	err = os.Chtimes(destination, time.Now(), lastModified) // Explicit to catch in deferred func
	if err != nil {
		return err
	}

	// change ownership and permissions
	if s.cfg.IncludePerm {
		if err = os.Chown(destination, perm.UID, perm.GID); err != nil {
			return err
		}
		err = syscall.Chmod(destination, uint32(perm.Mode))
	}
	return err
}

func (s *SyncManager) upload(ctx context.Context, rootPath string, remote *uri.URI, path string) error {
	source := filepath.Join(rootPath, path)
	if err := fileutil.VerifySafeFilename(source); err != nil {
		return err
	}
	remotePath := strings.TrimRight(remote.GetPath(), uri.PathSeparator)
	dest := strings.TrimPrefix(filepath.ToSlash(fmt.Sprintf("%s%s%s", remotePath, uri.PathSeparator, path)), uri.PathSeparator)

	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
	}()

	fileStat, err := f.Stat()
	if err != nil {
		return err
	}

	b := s.progressBar.AddReader(fmt.Sprintf("upload %s", path), fileStat.Size())
	defer func() {
		if err != nil {
			b.Error()
		} else {
			atomic.AddUint64(&s.tasks.Uploaded, 1)
			b.Done()
		}
	}()

	metadata := map[string]string{
		ClientMtimeMetadataKey: strconv.FormatInt(fileStat.ModTime().Unix(), 10),
	}

	reader := b.Reader(f)
	if s.cfg.IncludePerm {
		if strings.HasSuffix(path, uri.PathSeparator) { // Create a 0 byte reader for directories
			reader = bytes.NewReader([]byte{})
		}
		permissions, err := getPermissionFromFileInfo(fileStat)
		if err != nil {
			return err
		}
		data, err := json.Marshal(permissions)
		if err != nil {
			return err
		}
		metadata[POSIXPermissionsMetadataKey] = string(data)
	}

	readerWrapper := fileWrapper{
		file:   f,
		reader: reader,
	}
	if s.cfg.SyncFlags.Presign {
		_, err = helpers.ClientUploadPreSign(
			ctx, s.client, s.httpClient, remote.Repository, remote.Ref, dest, metadata, "", readerWrapper, s.cfg.SyncFlags.PresignMultipart)
		return err
	}
	// not pre-signed
	_, err = helpers.ClientUpload(
		ctx, s.client, remote.Repository, remote.Ref, dest, metadata, "", readerWrapper)
	return err
}

func (s *SyncManager) deleteLocal(rootPath string, change *Change) (err error) {
	b := s.progressBar.AddSpinner("delete local: " + change.Path)
	defer func() {
		defer func() {
			if err != nil {
				b.Error()
			} else {
				atomic.AddUint64(&s.tasks.Removed, 1)
				b.Done()
			}
		}()
	}()
	source := filepath.Join(rootPath, change.Path)
	err = fileutil.RemoveFile(source)
	if err != nil {
		return err
	}
	return nil
}

func (s *SyncManager) deleteRemote(ctx context.Context, remote *uri.URI, change *Change) (err error) {
	b := s.progressBar.AddSpinner("delete remote path: " + change.Path)
	defer func() {
		if err != nil {
			b.Error()
		} else {
			atomic.AddUint64(&s.tasks.Removed, 1)
			b.Done()
		}
	}()
	dest := filepath.ToSlash(filepath.Join(remote.GetPath(), change.Path))
	if strings.HasSuffix(change.Path, uri.PathSeparator) { // handle directory marker
		dest += uri.PathSeparator
	}
	resp, err := s.client.DeleteObjectWithResponse(ctx, remote.Repository, remote.Ref, &apigen.DeleteObjectParams{
		Path: dest,
	})
	if err != nil {
		return
	}
	if resp.StatusCode() != http.StatusNoContent {
		return fmt.Errorf("could not delete object: HTTP %d: %w", resp.StatusCode(), helpers.ErrRequestFailed)
	}
	return
}

func (s *SyncManager) Summary() Tasks {
	return s.tasks
}
