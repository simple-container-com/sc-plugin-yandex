// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package yandex

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/simple-container-com/api/pkg/api"
	yandex "github.com/simple-container-com/sc-plugin-yandex/api"
)

// fakeSiteS3 is an in-memory bucket.
type fakeSiteS3 struct {
	mu      sync.Mutex
	objects map[string]*s3.PutObjectInput
	bodies  map[string]string
	order   []string
	deniedN int // fail the first N calls with AccessDenied
}

func newFakeSiteS3(existing ...string) *fakeSiteS3 {
	f := &fakeSiteS3{objects: map[string]*s3.PutObjectInput{}, bodies: map[string]string{}}
	for _, k := range existing {
		f.objects[k] = &s3.PutObjectInput{Key: aws.String(k)}
	}
	return f
}

func (f *fakeSiteS3) denied() error {
	if f.deniedN > 0 {
		f.deniedN--
		return errors.New("operation error S3: PutObject, https response error StatusCode: 403, api error AccessDenied")
	}
	return nil
}

func (f *fakeSiteS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.denied(); err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(in.Body)
	k := aws.ToString(in.Key)
	f.objects[k] = in
	f.bodies[k] = string(body)
	f.order = append(f.order, k)
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeSiteS3) ListObjectsV2(_ context.Context, _ *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(false)}
	for k := range f.objects {
		out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k)})
	}
	return out, nil
}

func (f *fakeSiteS3) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range in.Delete.Objects {
		delete(f.objects, aws.ToString(o.Key))
	}
	return &s3.DeleteObjectsOutput{}, nil
}

func (f *fakeSiteS3) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ks []string
	for k := range f.objects {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func writeBundle(t *testing.T, files map[string]string) string {
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		Expect(os.MkdirAll(filepath.Dir(p), 0o755)).To(Succeed())
		Expect(os.WriteFile(p, []byte(body), 0o600)).To(Succeed())
	}
	return dir
}

func siteInput(bundleDir string) *yandex.StaticWebsiteInput {
	return &yandex.StaticWebsiteInput{
		StaticWebsiteTemplateConfig: yandex.StaticWebsiteTemplateConfig{
			AccountConfig: yandex.AccountConfig{CloudID: "test-cloud", FolderID: "test-folder"},
			ZoneName:      "simple-forge.ru",
			SpaFallback:   true,
		},
		Site:      api.StaticSiteConfig{Domain: "twopeaks.simple-forge.ru"},
		BundleDir: bundleDir,
	}
}

// The deploy-yc.sh contract: hashed bundles immutable, index no-cache, the rest an
// hour; HTML served with a charset; objects no longer in the build removed.
func TestSyncSite_UploadsWithHeadersAndDeletesStale(t *testing.T) {
	RegisterTestingT(t)

	dir := writeBundle(t, map[string]string{
		"index.html":           "<!doctype html><title>Две вершины</title>",
		"assets/index-3f9a.js": "console.log(1)",
		"images/peak.jpg":      "\xff\xd8\xff\xe0jpeg",
		"robots.txt":           "User-agent: *",
	})
	s3c := newFakeSiteS3("assets/index-old.js", "index.html")
	res, err := syncSite(context.Background(), s3c, "twopeaks.simple-forge.ru", siteInput(dir))
	Expect(err).ToNot(HaveOccurred())
	Expect(res).To(Equal(siteSyncResult{Uploaded: 4, Deleted: 1}))
	Expect(s3c.keys()).To(Equal([]string{"assets/index-3f9a.js", "images/peak.jpg", "index.html", "robots.txt"}))

	obj := func(k string) *s3.PutObjectInput { return s3c.objects[k] }
	Expect(aws.ToString(obj("index.html").CacheControl)).To(Equal(yandex.CacheControlNoCache))
	Expect(aws.ToString(obj("index.html").ContentType)).To(Equal("text/html; charset=utf-8"))
	Expect(aws.ToString(obj("assets/index-3f9a.js").CacheControl)).To(Equal(yandex.CacheControlImmutable))
	Expect(aws.ToString(obj("images/peak.jpg").CacheControl)).To(Equal(yandex.CacheControlDefault))
	Expect(aws.ToString(obj("images/peak.jpg").ContentType)).To(Equal("image/jpeg"))
	Expect(aws.ToString(obj("robots.txt").ContentType)).To(HavePrefix("text/plain"))
	Expect(s3c.bodies["index.html"]).To(ContainSubstring("Две вершины"))

	// The index goes last, so it never references a bundle not yet uploaded.
	Expect(s3c.order[len(s3c.order)-1]).To(Equal("index.html"))
}

func TestSyncSite_RefusesABundleWithoutIndex(t *testing.T) {
	RegisterTestingT(t)

	dir := writeBundle(t, map[string]string{"assets/a.js": "x"})
	s3c := newFakeSiteS3("index.html")
	_, err := syncSite(context.Background(), s3c, "b", siteInput(dir))
	Expect(err).To(MatchError(ContainSubstring("has no index.html")))
	Expect(s3c.keys()).To(Equal([]string{"index.html"}), "nothing is deleted from a live site when the build is broken")

	_, err = syncSite(context.Background(), s3c, "b", siteInput(filepath.Join(dir, "missing")))
	Expect(err).To(HaveOccurred())
}

// The uploader's bucket binding is created in the same deploy, and YC IAM takes a
// moment to honour it.
func TestSyncSite_RetriesAccessDenied(t *testing.T) {
	RegisterTestingT(t)

	dir := writeBundle(t, map[string]string{"index.html": "x"})
	s3c := newFakeSiteS3()
	s3c.deniedN = 1
	_, err := syncSite(context.Background(), s3c, "b", siteInput(dir))
	Expect(err).ToNot(HaveOccurred())
	Expect(s3c.keys()).To(Equal([]string{"index.html"}))
}

func TestSiteNames(t *testing.T) {
	RegisterTestingT(t)

	Expect(siteResourceName("TwoPeaks.Simple-Forge.ru")).To(Equal("twopeaks-simple-forge-ru"))
	Expect(serviceAccountName("twopeaks-simple-forge-ru-uploader")).To(Equal("twopeaks-simple-forge-ru-uploader"))
	Expect(serviceAccountName("1st-simple-forge-ru-uploader")).To(HavePrefix("sa-1st"))
	long := serviceAccountName(strings.Repeat("a", 62) + "-b")
	Expect(len(long)).To(BeNumerically("<=", 63))
	Expect(long).ToNot(HaveSuffix("-"))
	Expect(certificateName("twopeaks-simple-forge-ru")).To(Equal("twopeaks-simple-forge-ru-cert"))
}

// siteMocks answers the zone and certificate lookups StaticWebsite makes.
type siteMocks struct {
	bucketMocks
	calls []string
}

func (m *siteMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	m.mu.Lock()
	m.calls = append(m.calls, args.Token)
	m.mu.Unlock()
	switch args.Token {
	case "yandex:index/getDnsZone:getDnsZone":
		return resource.PropertyMap{
			"id":   resource.NewStringProperty("dnszone-1"),
			"name": resource.NewStringProperty("simple-forge-ru"),
			"zone": resource.NewStringProperty("simple-forge.ru."),
		}, nil
	case "yandex:index/getCmCertificate:getCmCertificate":
		out := args.Args.Copy()
		if _, ok := out["id"]; !ok {
			id := "fpq-looked-up"
			if v, ok := args.Args["certificateId"]; ok && v.IsString() {
				id = v.StringValue()
			}
			out["id"] = resource.NewStringProperty(id)
		}
		out["status"] = resource.NewStringProperty("ISSUED")
		return out, nil
	}
	return args.Args, nil
}

func (m *siteMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	id, state, err := m.bucketMocks.NewResource(args)
	if args.TypeToken == "yandex:index/cmCertificate:CmCertificate" {
		state = state.Copy()
		state["challenges"] = resource.NewArrayProperty([]resource.PropertyValue{
			resource.NewObjectProperty(resource.PropertyMap{
				"dnsName":  resource.NewStringProperty("_acme-challenge.twopeaks.simple-forge.ru."),
				"dnsType":  resource.NewStringProperty("CNAME"),
				"dnsValue": resource.NewStringProperty("fpq1.cm.yandexcloud.net."),
			}),
		})
	}
	if args.TypeToken == "yandex:index/iamServiceAccountStaticAccessKey:IamServiceAccountStaticAccessKey" {
		state = state.Copy()
		state["accessKey"] = resource.NewStringProperty("AKID")
		state["secretKey"] = resource.NewStringProperty("SECRET")
	}
	return id, state, err
}

func runStaticWebsite(t *testing.T, cfg *yandex.StaticWebsiteInput, dryRun bool) (*siteMocks, *fakeSiteS3, error) {
	mocks := &siteMocks{}
	fake := newFakeSiteS3("stale.html")
	prev := newSiteS3Client
	newSiteS3Client = func(_, _, _ string) siteS3 { return fake }
	t.Cleanup(func() { newSiteS3Client = prev })

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		params, err := bucketProvisionParams(ctx)
		if err != nil {
			return err
		}
		_, err = StaticWebsite(ctx, api.Stack{Name: "two-peaks"}, api.ResourceInput{
			Descriptor:  &api.ResourceDescriptor{Type: yandex.TemplateTypeYandexStaticWebsite, Name: "two-peaks", Config: api.Config{Config: cfg}},
			StackParams: &api.StackParams{Environment: "prod"},
		}, params)
		return err
	}, pulumi.WithMocks("project", "stack", mocks), withDryRun(dryRun))
	return mocks, fake, err
}

func withDryRun(dryRun bool) pulumi.RunOption {
	return func(info *pulumi.RunInfo) { info.DryRun = dryRun }
}

func TestStaticWebsite_AdoptedCertificateByName(t *testing.T) {
	RegisterTestingT(t)

	cfg := siteInput(writeBundle(t, map[string]string{"index.html": "x", "assets/a-1.js": "y"}))
	cfg.CertificateName = "simple-forge-ru-cert"
	mocks, fake, err := runStaticWebsite(t, cfg, false)
	Expect(err).ToNot(HaveOccurred())

	bucket := mocks.inputsOf("yandex:index/storageBucket:StorageBucket")
	Expect(bucket["bucket"].StringValue()).To(Equal("twopeaks.simple-forge.ru"), "bucket-hosted HTTPS needs bucket name == host")
	Expect(bucket["forceDestroy"].BoolValue()).To(BeTrue())
	flags := bucket["anonymousAccessFlags"].ObjectValue()
	Expect(flags["read"].BoolValue()).To(BeTrue())
	Expect(flags["list"].BoolValue()).To(BeFalse())
	website := bucket["website"].ObjectValue()
	Expect(website["indexDocument"].StringValue()).To(Equal("index.html"))
	Expect(website["errorDocument"].StringValue()).To(Equal("index.html"))
	Expect(bucket["https"].ObjectValue()["certificateId"].StringValue()).To(Equal("fpq-looked-up"))

	Expect(mocks.countOf("yandex:index/cmCertificate:CmCertificate")).To(Equal(0), "an adopted certificate is never created")
	Expect(mocks.countOf("yandex:index/dnsRecordset:DnsRecordset")).To(Equal(1))
	cname := mocks.inputsOf("yandex:index/dnsRecordset:DnsRecordset")
	Expect(cname["zoneId"].StringValue()).To(Equal("dnszone-1"))
	Expect(cname["name"].StringValue()).To(Equal("twopeaks.simple-forge.ru."))
	Expect(cname["type"].StringValue()).To(Equal("CNAME"))
	Expect(cname["datas"].ArrayValue()[0].StringValue()).To(Equal("twopeaks.simple-forge.ru.website.yandexcloud.net."))

	binding := mocks.inputsOf("yandex:index/storageBucketIamBinding:StorageBucketIamBinding")
	Expect(binding["role"].StringValue()).To(Equal("storage.editor"))
	Expect(mocks.countOf("yandex:index/resourcemanagerFolderIamMember:ResourcemanagerFolderIamMember")).To(Equal(0),
		"the uploader's role is on its bucket, not the folder")

	Expect(fake.keys()).To(Equal([]string{"assets/a-1.js", "index.html"}), "uploaded, and the stale object deleted")
}

func TestStaticWebsite_ManagedCertificateWithDnsChallenge(t *testing.T) {
	RegisterTestingT(t)

	cfg := siteInput(writeBundle(t, map[string]string{"index.html": "x"}))
	mocks, _, err := runStaticWebsite(t, cfg, false)
	Expect(err).ToNot(HaveOccurred())

	cert := mocks.inputsOf("yandex:index/cmCertificate:CmCertificate")
	Expect(cert).ToNot(BeNil())
	Expect(cert["domains"].ArrayValue()[0].StringValue()).To(Equal("twopeaks.simple-forge.ru"))
	Expect(cert["managed"].ObjectValue()["challengeType"].StringValue()).To(Equal("DNS_CNAME"))

	Expect(mocks.countOf("yandex:index/dnsRecordset:DnsRecordset")).To(Equal(2), "the site CNAME and the ACME challenge")
	var acme resource.PropertyMap
	mocks.mu.Lock()
	for _, a := range mocks.created {
		if a.TypeToken == "yandex:index/dnsRecordset:DnsRecordset" && a.Name == "twopeaks-simple-forge-ru-acme" {
			acme = a.Inputs
		}
	}
	mocks.mu.Unlock()
	Expect(acme["name"].StringValue()).To(Equal("_acme-challenge.twopeaks.simple-forge.ru."))
	Expect(acme["datas"].ArrayValue()[0].StringValue()).To(Equal("fpq1.cm.yandexcloud.net."))

	// The bucket takes the id from the waitValidation lookup, not straight from the
	// new certificate: attaching an unvalidated certificate is refused.
	Expect(mocks.calls).To(ContainElement("yandex:index/getCmCertificate:getCmCertificate"))
	bucket := mocks.inputsOf("yandex:index/storageBucket:StorageBucket")
	Expect(bucket["https"].ObjectValue()["certificateId"].StringValue()).To(Equal("twopeaks-simple-forge-ru-cert-id"))
}

func TestStaticWebsite_PreviewDoesNotUpload(t *testing.T) {
	RegisterTestingT(t)

	cfg := siteInput(writeBundle(t, map[string]string{"index.html": "x"}))
	cfg.CertificateID = "fpq-fixed"
	mocks, fake, err := runStaticWebsite(t, cfg, true)
	Expect(err).ToNot(HaveOccurred())
	Expect(mocks.countOf("yandex:index/storageBucket:StorageBucket")).To(Equal(1))
	Expect(fake.keys()).To(Equal([]string{"stale.html"}), "a preview neither uploads nor deletes")
}

func TestStaticWebsite_RejectsInvalidConfigBeforeCreatingAnything(t *testing.T) {
	RegisterTestingT(t)

	cfg := siteInput(t.TempDir())
	cfg.Site.Domain = "twopeaks.example.com"
	mocks, _, err := runStaticWebsite(t, cfg, false)
	Expect(err).To(MatchError(ContainSubstring("not inside zone")))
	Expect(mocks.countOf("yandex:index/storageBucket:StorageBucket")).To(Equal(0))
}
