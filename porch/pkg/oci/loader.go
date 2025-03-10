// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package oci

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	api "github.com/GoogleContainerTools/kpt/porch/api/porch/v1alpha1"
	"github.com/GoogleContainerTools/kpt/porch/pkg/repository"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/klog/v2"
)

var tracer = otel.Tracer("oci")

func (r *ociRepository) loadTasks(ctx context.Context, imageRef ImageDigestName) ([]api.Task, error) {
	ctx, span := tracer.Start(ctx, "ociRepository::loadTasks", trace.WithAttributes(
		attribute.Stringer("image", imageRef),
	))
	defer span.End()

	configFile, err := r.storage.cachedConfigFile(ctx, imageRef)
	if err != nil {
		return nil, fmt.Errorf("error fetching config for image: %w", err)
	}

	var tasks []api.Task
	for i := range configFile.History {
		history := &configFile.History[i]
		command := history.CreatedBy
		if strings.HasPrefix(command, "kpt:") {
			task := api.Task{}
			b := []byte(strings.TrimPrefix(command, "kpt:"))
			if err := json.Unmarshal(b, &task); err != nil {
				klog.Warningf("failed to unmarshal task command %q: %w", command, err)
				continue
			}
			tasks = append(tasks, task)
		} else {
			klog.Warningf("unknown task command in history %q", command)
		}
	}

	return tasks, nil
}

func (r *Storage) LookupImageTag(ctx context.Context, imageName ImageTagName) (*ImageDigestName, error) {
	ctx, span := tracer.Start(ctx, "Storage::LookupImageTag", trace.WithAttributes(
		attribute.Stringer("image", imageName),
	))
	defer span.End()

	ociImage, err := r.toRemoteImage(ctx, imageName)
	if err != nil {
		return nil, err
	}

	digest, err := ociImage.Digest()
	if err != nil {
		return nil, err
	}

	return &ImageDigestName{
		Image:  imageName.Image,
		Digest: digest.String(),
	}, nil
}

func (r *Storage) LoadResources(ctx context.Context, imageName *ImageDigestName) (*repository.PackageResources, error) {
	ctx, span := tracer.Start(ctx, "Storage::loadResources", trace.WithAttributes(
		attribute.Stringer("image", imageName),
	))
	defer span.End()

	if imageName.Digest == "" {
		// New package; no digest yet
		return &repository.PackageResources{
			Contents: map[string]string{},
		}, nil
	}

	fetcher := func() (io.ReadCloser, error) {
		ociImage, err := r.toRemoteImage(ctx, imageName)
		if err != nil {
			return nil, err
		}

		reader := mutate.Extract(ociImage)
		return reader, nil
	}

	// We need the per-digest cache here because otherwise we have to make a network request to look up the manifest in remote.Image
	// (this could be cached by the go-containerregistry library, for some reason it is not...)
	// TODO: Is there then any real reason to _also_ have the image-layer cache?
	f, err := withCacheFile(filepath.Join(r.cacheDir, "resources", imageName.Digest), fetcher)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	tarReader := tar.NewReader(f)

	// TODO: Check hash here?  Or otherwise handle error?
	resources, err := loadResourcesFromTar(ctx, tarReader)
	if err != nil {
		return nil, err
	}
	return resources, nil
}

func loadResourcesFromTar(ctx context.Context, tarReader *tar.Reader) (*repository.PackageResources, error) {
	resources := &repository.PackageResources{
		Contents: map[string]string{},
	}

	for {
		hdr, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		path := hdr.Name
		fileType := hdr.FileInfo().Mode().Type()
		switch fileType {
		case fs.ModeDir:
			// Ignored
		case fs.ModeSymlink:
			// We probably don't want to support this; feels high-risk, low-reward
			return nil, fmt.Errorf("package cannot contain symlink (%q)", path)
		case 0:
			b, err := io.ReadAll(tarReader)
			if err != nil {
				return nil, fmt.Errorf("error reading %q from image: %w", path, err)
			}
			resources.Contents[path] = string(b)

		default:
			return nil, fmt.Errorf("package cannot unsupported entry type for %q (%v)", path, fileType)

		}
	}

	return resources, nil
}

// withCacheFile runs with a filesystem-backed cache.
// If cacheFilePath does not exist, it will be fetched with the function fetcher.
// The file contents are then processed with the function reader.
// TODO: We likely need some form of GC/LRU on the cache file paths.
// We can probably use FS access time (or we might need to touch the files when we access them)!
func withCacheFile(cacheFilePath string, fetcher func() (io.ReadCloser, error)) (io.ReadCloser, error) {
	dir := filepath.Dir(cacheFilePath)

	f, err := os.Open(cacheFilePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			f = nil
		} else {
			return nil, fmt.Errorf("error opening cache file %q: %w", cacheFilePath, err)
		}
	} else {
		// TODO: Delete file if corrupt?
		return f, nil
	}

	r, err := fetcher()
	if err != nil {
		return nil, err
	}
	defer r.Close()

	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory %q: %w", dir, err)
	}

	tempFile, err := os.CreateTemp(dir, "")
	if err != nil {
		return nil, fmt.Errorf("failed to create tempfile in directory %q: %w", dir, err)
	}
	defer func() {
		if tempFile != nil {
			if err := tempFile.Close(); err != nil {
				klog.Warningf("error closing temp file: %v", err)
			}

			if err := os.Remove(tempFile.Name()); err != nil {
				klog.Warningf("failed to write tempfile: %v", err)
			}
		}
	}()
	if _, err := io.Copy(tempFile, r); err != nil {
		return nil, fmt.Errorf("error caching data: %w", err)
	}

	if err := tempFile.Close(); err != nil {
		return nil, fmt.Errorf("error closing temp file: %w", err)
	}

	if err := os.Rename(tempFile.Name(), cacheFilePath); err != nil {
		return nil, fmt.Errorf("error renaming temp file %q -> %q: %w", tempFile.Name(), cacheFilePath, err)
	}

	tempFile = nil

	f, err = os.Open(cacheFilePath)
	if err != nil {
		return nil, fmt.Errorf("error opening cache file %q (after fetch): %w", cacheFilePath, err)
	}
	return f, nil
}
