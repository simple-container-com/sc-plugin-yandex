// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"bytes"
	"context"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/pkg/errors"

	yandex "github.com/simple-container-com/sc-plugin-yandex/api"
)

// siteS3 is the slice of the S3 API the site upload uses; a fake implements it in
// tests.
type siteS3 interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// newSiteS3 builds an S3 client for YC Object Storage from a static access key.
//
// Path-style addressing is not optional here: the bucket is named after the
// site's domain, so it contains dots, and a virtual-hosted request to
// `twopeaks.simple-forge.ru.storage.yandexcloud.net` fails TLS — the endpoint's
// wildcard certificate covers one label, not three.
func newSiteS3(region, accessKey, secretKey string) siteS3 {
	return s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(yandex.DefaultStorageEndpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		UsePathStyle: true,
	})
}

// newSiteS3Client is the constructor StaticWebsite uses; tests swap in a fake.
var newSiteS3Client = newSiteS3

// siteFile is one file of the bundle, keyed the way it is served.
type siteFile struct {
	Key  string
	Path string
}

// listSiteFiles walks bundleDir and returns every regular file with its object key
// (slash-separated, relative to bundleDir), sorted for a stable upload order.
func listSiteFiles(bundleDir string) ([]siteFile, error) {
	info, err := os.Stat(bundleDir)
	if err != nil {
		return nil, errors.Wrapf(err, "bundleDir %q", bundleDir)
	}
	if !info.IsDir() {
		return nil, errors.Errorf("bundleDir %q is not a directory", bundleDir)
	}
	var files []siteFile
	err = filepath.WalkDir(bundleDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(bundleDir, p)
		if err != nil {
			return err
		}
		files = append(files, siteFile{Key: filepath.ToSlash(rel), Path: p})
		return nil
	})
	if err != nil {
		return nil, errors.Wrapf(err, "walk bundleDir %q", bundleDir)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Key < files[j].Key })
	return files, nil
}

// contentTypeFor names the object's Content-Type. Text types carry an explicit
// charset: Object Storage serves what it was told, and a Cyrillic page served as
// bare `text/html` renders as mojibake in any browser that guesses Latin-1.
func contentTypeFor(key string, head []byte) string {
	ct := mime.TypeByExtension(strings.ToLower(path.Ext(key)))
	if ct == "" {
		ct = http.DetectContentType(head)
	}
	if strings.HasPrefix(ct, "text/") && !strings.Contains(ct, "charset") {
		ct += "; charset=utf-8"
	}
	return ct
}

// siteSyncResult summarises an upload for the deploy log.
type siteSyncResult struct {
	Uploaded int
	Deleted  int
}

// siteUploadConcurrency bounds parallel PUTs. Object Storage accepts far more; the
// bound is about not opening hundreds of files at once on a CI runner.
const siteUploadConcurrency = 8

// accessRetryWindow is how long an AccessDenied is retried. The uploader's key
// belongs to a service account whose bucket role binding is created in the same
// deploy, and YC IAM takes a few seconds to honour a fresh binding.
var accessRetryWindow = 90 * time.Second

// syncSite makes the bucket hold exactly the bundle: every file is uploaded with
// its Cache-Control and Content-Type, and every object not in the bundle is
// deleted — the `aws s3 sync --delete` of the hand-written deploy.
//
// Every file is uploaded on every deploy rather than only changed ones. Skipping by
// ETag would miss a Cache-Control rule change (headers are not part of the ETag),
// and a site is small enough that re-uploading it is cheaper than being wrong.
//
// The index document goes last, after every other file is in place and before the
// stale ones are deleted, so a visitor never gets an index referencing a bundle
// that is not there yet, nor one whose bundle was just deleted.
func syncSite(ctx context.Context, client siteS3, bucket string, in *yandex.StaticWebsiteInput) (siteSyncResult, error) {
	var res siteSyncResult
	files, err := listSiteFiles(in.EffectiveBundleDir())
	if err != nil {
		return res, err
	}
	indexKey := in.EffectiveIndexDocument()
	var index *siteFile
	rest := make([]siteFile, 0, len(files))
	for i := range files {
		if files[i].Key == indexKey {
			index = &files[i]
			continue
		}
		rest = append(rest, files[i])
	}
	if index == nil {
		return res, errors.Errorf("bundleDir %q has no %s — build the site before deploying", in.EffectiveBundleDir(), indexKey)
	}

	if err := uploadAll(ctx, client, bucket, in, rest); err != nil {
		return res, err
	}
	if err := uploadOne(ctx, client, bucket, in, *index); err != nil {
		return res, err
	}
	res.Uploaded = len(files)

	keep := make(map[string]bool, len(files))
	for _, f := range files {
		keep[f.Key] = true
	}
	deleted, err := deleteStale(ctx, client, bucket, keep)
	res.Deleted = deleted
	return res, err
}

func uploadAll(ctx context.Context, client siteS3, bucket string, in *yandex.StaticWebsiteInput, files []siteFile) error {
	sem := make(chan struct{}, siteUploadConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, f := range files {
		mu.Lock()
		failed := firstErr != nil
		mu.Unlock()
		if failed {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(f siteFile) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := uploadOne(ctx, client, bucket, in, f); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(f)
	}
	wg.Wait()
	return firstErr
}

func uploadOne(ctx context.Context, client siteS3, bucket string, in *yandex.StaticWebsiteInput, f siteFile) error {
	body, err := os.ReadFile(f.Path)
	if err != nil {
		return errors.Wrapf(err, "read %q", f.Path)
	}
	head := body
	if len(head) > 512 {
		head = head[:512]
	}
	input := &s3.PutObjectInput{
		Bucket:       aws.String(bucket),
		Key:          aws.String(f.Key),
		CacheControl: aws.String(in.CacheControlFor(f.Key)),
		ContentType:  aws.String(contentTypeFor(f.Key, head)),
	}
	return withAccessRetry(ctx, func() error {
		input.Body = bytes.NewReader(body)
		_, err := client.PutObject(ctx, input)
		return errors.Wrapf(err, "upload %q to bucket %q", f.Key, bucket)
	})
}

// deleteStale removes every object whose key is not in keep.
func deleteStale(ctx context.Context, client siteS3, bucket string, keep map[string]bool) (int, error) {
	var stale []s3types.ObjectIdentifier
	var token *string
	for {
		var out *s3.ListObjectsV2Output
		err := withAccessRetry(ctx, func() error {
			var err error
			out, err = client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), ContinuationToken: token})
			return errors.Wrapf(err, "list bucket %q", bucket)
		})
		if err != nil {
			return 0, err
		}
		for _, obj := range out.Contents {
			if key := aws.ToString(obj.Key); !keep[key] {
				stale = append(stale, s3types.ObjectIdentifier{Key: aws.String(key)})
			}
		}
		if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			break
		}
		token = out.NextContinuationToken
	}
	// DeleteObjects takes at most 1000 keys per call.
	for start := 0; start < len(stale); start += 1000 {
		end := min(start+1000, len(stale))
		out, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &s3types.Delete{Objects: stale[start:end], Quiet: aws.Bool(true)},
		})
		if err != nil {
			return start, errors.Wrapf(err, "delete stale objects from bucket %q", bucket)
		}
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return start, errors.Errorf("delete stale object %q from bucket %q: %s %s",
				aws.ToString(e.Key), bucket, aws.ToString(e.Code), aws.ToString(e.Message))
		}
	}
	return len(stale), nil
}

// withAccessRetry retries fn while it fails with AccessDenied, for up to
// accessRetryWindow. Any other error returns at once.
func withAccessRetry(ctx context.Context, fn func() error) error {
	deadline := time.Now().Add(accessRetryWindow)
	delay := 2 * time.Second
	for {
		err := fn()
		if err == nil || !isAccessDenied(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
		if delay < 10*time.Second {
			delay *= 2
		}
	}
}

func isAccessDenied(err error) bool {
	s := err.Error()
	return strings.Contains(s, "AccessDenied") || strings.Contains(s, "StatusCode: 403")
}
