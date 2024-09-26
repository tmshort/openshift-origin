package operatorsv1

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	configv1 "github.com/openshift/api/config/v1"
	exutil "github.com/openshift/origin/test/extended/util"
)

const (
	operatorsGroupName = "olm.operatorframework.io"
	packagesGroupName  = "packages." + operatorsGroupName
)

var _ = g.Describe("[sig-olmv1] OLMv1 CRDs", func() {
	defer g.GinkgoRecover()
	oc := exutil.NewCLIWithoutNamespace("default")

	g.It("should be installed", func(ctx g.SpecContext) {
		// Check for tech preview, if this is not tech preview, bail
		if !exutil.IsTechPreviewNoUpgrade(ctx, oc.AdminConfigClient()) {
			g.Skip("Test only runs in tech-preview")
		}

		// supports multiple versions during transision
		providedAPIs := []struct {
			group   string
			version []string
			plural  string
		}{
			{
				group:   operatorsGroupName,
				version: []string{"v1alpha1", "v1"},
				plural:  "clusterextensions",
			},
			{
				group:   operatorsGroupName,
				version: []string{"v1alpha1", "v1"},
				plural:  "clustercatalogs",
			},
		}

		for _, api := range providedAPIs {
			g.By(fmt.Sprintf("checking %s at version %s [apigroup:%s]", api.plural, api.version, api.group))
			// Ensure expected version exists in spec.versions and is both served and stored
			var err error
			var raw string
			for _, ver := range api.version {
				raw, err = oc.AsAdmin().Run("get").Args("crds", fmt.Sprintf("%s.%s", api.plural, api.group), fmt.Sprintf("-o=jsonpath={.spec.versions[?(@.name==%q)]}", ver)).Output()
				if err == nil {
					break
				}
			}
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(raw).To(o.MatchRegexp(`served.?:true`))
			o.Expect(raw).To(o.MatchRegexp(`storage.?:true`))
		}
	})
})

var _ = g.Describe("[sig-olmv1] OLMv1 operator installation", func() {
	defer g.GinkgoRecover()

	var (
		baseDir            = exutil.FixturePath("testdata", "olmv1")
		imageStreamFile    = filepath.Join(baseDir, "catalog-image-stream.json")
		buildConfigFile    = filepath.Join(baseDir, "catalog-build-config.json")
		catalogDockerDir   = filepath.Join(baseDir, "catalog")
		clusterCatalogFile = filepath.Join(baseDir, "catalog.yaml")
	)
	oc := exutil.NewCLI("openshift-operator-controller")

	// Check for tech preview, if this is not tech preview, bail
	if !exutil.IsTechPreviewNoUpgrade(context.Background(), oc.AdminConfigClient()) {
		g.Skip("Test only runs in tech-preview")
	}

	g.BeforeEach(func() {
		exutil.PreTestDump()

		// THIS NEEDS TO BE DONE IN "openshift-catalogd"
		g.By("copying the cluster pull secret to the namespace")
		ps, err := oc.AdminKubeClient().CoreV1().Secrets("openshift-config").Get(context.Background(), "pull-secret", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		localPullSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "local-ps",
			},
			Data: ps.Data,
			Type: ps.Type,
		}
		_, err = oc.AdminKubeClient().CoreV1().Secrets("openshift-catalogd").Create(context.Background(), localPullSecret, metav1.CreateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("linking pull secret with the builder service account")
		err = oc.AsAdmin().WithoutNamespace().Run("secrets").Args("-n", "openshift-catalogd", "link", "catalogd-controller-manager", "local-ps").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("adding image-pull permissions to catalogd")
		err = oc.AsAdmin().WithoutNamespace().Run("policy").
			Args("-n", "openshift-catalogd", "add-role-to-user", "system:image-puller",
				"system:serviceaccount:openshift-catalogd:catalogd-controller-manager").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	g.AfterEach(func() {
		if g.CurrentSpecReport().Failed() {
			exutil.DumpPodLogsStartingWith("", oc)
		}
		oc.AdminConfigClient().ConfigV1().ImageTagMirrorSets().Delete(context.Background(), "catalog-test", metav1.DeleteOptions{})
		oc.AsAdmin().WithoutNamespace().Run("delete").Args("-f", clusterCatalogFile).Execute()
		oc.AsAdmin().Run("delete").Args("-f", buildConfigFile).Execute()
		oc.AsAdmin().Run("delete").Args("-f", imageStreamFile).Execute()

		g.By("unlinking the cluster pull secret in the namespace")
		oc.AsAdmin().WithoutNamespace().Run("secrets").Args("-n", "openshift-catalogd", "unlink", "catalogd-controller-manager", "local-ps").Execute()

		g.By("deleting the cluster pull secret in the namespace")
		oc.AsAdmin().WithoutNamespace().Run("delete").Args("-n", "openshift-catalogd", "secret", "local-ps").Execute()
	})

	g.It("should create a catalog", func() {
		g.By("creating the catalog image stream")
		err := oc.AsAdmin().Run("create").Args("-f", imageStreamFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("creating the catalog build config")
		err = oc.AsAdmin().Run("create").Args("-f", buildConfigFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("building the catalog source image")
		br, _ := exutil.StartBuildAndWait(oc, "catalog-test", "--from-dir", catalogDockerDir)
		br.AssertSuccess()
		o.Expect(br.Logs()).To(o.MatchRegexp(`pushed image-registry\.openshift-image-registry\.svc:5000/.*/catalog-test@sha256:`))

		g.By("determining the image name in the image-registry")
		// extract the image
		//with sha hash: imageRegex := regexp.MustCompile(`image-registry\.openshift-image-registry\.svc:5000/.*/catalog@sha256:[a-z0-9]+`)
		imageRegex := regexp.MustCompile(`image-registry\.openshift-image-registry\.svc:5000/[-a-z0-9]+/catalog-test`)
		logs, err := br.Logs()
		o.Expect(err).NotTo(o.HaveOccurred())
		imageLocation := imageRegex.FindString(logs)
		g.GinkgoWriter.Printf("IMAGE NAME: %q\n", imageLocation)

		g.By("creating an ITMS")
		itms := newImageTagMirrorSet(imageLocation)
		_, err = oc.AdminConfigClient().ConfigV1().ImageTagMirrorSets().Create(context.Background(), itms, metav1.CreateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("creating the ClusterCatalog")
		err = oc.AsAdmin().WithoutNamespace().Run("create").Args("-f", clusterCatalogFile).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("waiting for the ClusterCatalog to be serving")
		err = wait.PollUntilContextTimeout(context.Background(), time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
			var conditions []metav1.Condition
			output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("clustercatalogs.olm.operatorframework.io", "catalog-test", "-o=jsonpath={.status.conditions}").Output()
			if err != nil {
				return false, err
			}
			// no data yet, so try again
			if output == "" {
				return false, nil
			}
			err = json.Unmarshal([]byte(output), &conditions)
			if err != nil {
				return false, fmt.Errorf("error in json.Unmarshal(%v): %v", output, err)
			}
			if !meta.IsStatusConditionPresentAndEqual(conditions, "Progressing", metav1.ConditionFalse) {
				return false, nil
			}
			if !meta.IsStatusConditionPresentAndEqual(conditions, "Serving", metav1.ConditionTrue) {
				return false, nil
			}
			return true, nil
		})
		o.Expect(err).NotTo(o.HaveOccurred())
		// The following is the ending status. It does not seem to be able to get the image?
		//status:
		//  conditions:
		//  - lastTransitionTime: "2024-10-04T20:51:30Z"
		//    message: 'source bundle content: error resolving canonical reference: error creating
		//      image source: reading manifest latest in quay.io/operatorframework/catalog-test:
		//      unauthorized: access to the requested resource is not authorized'
		//    reason: Retrying
		//    status: "True"
		//    type: Progressing
	})
})

func newImageTagMirrorSet(image string) *configv1.ImageTagMirrorSet {
	return &configv1.ImageTagMirrorSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "catalog-test",
		},
		Spec: configv1.ImageTagMirrorSetSpec{
			ImageTagMirrors: []configv1.ImageTagMirrors{
				{
					MirrorSourcePolicy: configv1.NeverContactSource,
					Source:             "quay.io/operatorframework/catalog-test",
					Mirrors: []configv1.ImageMirror{
						configv1.ImageMirror(image),
					},
				},
			},
		},
	}
}

var _ = g.Describe("[sig-olmv1] OLMv1 should have access to certain files", func() {
	defer g.GinkgoRecover()

	var oc = exutil.NewCLIWithoutNamespace("default")

	g.It("checks the manager containers for those files", func(ctx g.SpecContext) {
		oc := oc

		// Check for tech preview, if this is not tech preview, bail
		if !exutil.IsTechPreviewNoUpgrade(ctx, oc.AdminConfigClient()) {
			g.Skip("Test only runs in tech-preview")
		}

		pods := []string{
			"catalogd",
			"operator-controller",
		}

		for _, v := range pods {
			namespace := fmt.Sprintf("openshift-%s", v)
			label := fmt.Sprintf("%s-controller-manager", v)

			controlPlaneTopology, err := exutil.GetControlPlaneTopology(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			if *controlPlaneTopology == configv1.ExternalTopologyMode {
				_, namespace, err = exutil.GetHypershiftManagementClusterConfigAndNamespace()
				o.Expect(err).NotTo(o.HaveOccurred())
				oc = exutil.NewHypershiftManagementCLI("default").AsAdmin().WithoutNamespace()
			}

			podName, err := oc.AsAdmin().Run("get").Args("-n", namespace, "pods", "-l", fmt.Sprintf("control-plane=%s", label), "-o=jsonpath={.items[0].metadata.name}").Output()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By(fmt.Sprintf("checking for /etc/containers in %q", podName))
			oc.SetNamespace(namespace)
			_, err = oc.AsAdmin().Run("exec").Args("-n", namespace, podName, "-c", "manager", "--", "ls", "/etc/containers").Output()
			o.Expect(err).NotTo(o.HaveOccurred())

			// e2e.Logf("olm source git commit ID:%s", gitCommitID)
			// e2e.Failf(fmt.Sprintf("the length of the git commit id is %d, != 40", len(gitCommitID)))
		}
	})
})
