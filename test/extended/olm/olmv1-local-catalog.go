package operators

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	configv1 "github.com/openshift/api/config/v1"
	exutil "github.com/openshift/origin/test/extended/util"
)

var _ = g.Describe("[sig-olmv2][OCPFeatureGate:NewOLM] OLMv1 local operator installation", func() {
	defer g.GinkgoRecover()

	var (
		baseDir            = exutil.FixturePath("testdata", "olmv1")
		imageStreamFile    = filepath.Join(baseDir, "catalog-image-stream.json")
		buildConfigFile    = filepath.Join(baseDir, "catalog-build-config.json")
		catalogDockerDir   = filepath.Join(baseDir, "catalog")
		clusterCatalogFile = filepath.Join(baseDir, "catalog.yaml")
	)
	oc := exutil.NewCLI("openshift-operator-controller")

	g.BeforeEach(func() {
		exutil.PreTestDump()

		// THIS NEEDS TO BE DONE IN "openshift-catalogd"
		g.By("getting the list of secrets in the namespace")
		secrets, err := oc.AdminKubeClient().CoreV1().Secrets("openshift-catalogd").List(context.Background(), metav1.ListOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		for _, secret := range secrets.Items {
			if strings.HasPrefix(secret.Name, "builder-dockercfg") {
				g.By(fmt.Sprintf("linking pull secret %s with the catalogd service account", secret.Name))
				err = oc.AsAdmin().WithoutNamespace().Run("secrets").Args("-n", "openshift-catalogd", "link", "catalogd-controller-manager", secret.Name, "--for=pull").Execute()
				o.Expect(err).NotTo(o.HaveOccurred())
			}
		}

		g.By("adding image-pull permissions to catalogd")
		err = oc.AsAdmin().Run("adm").
			Args("policy", "add-cluster-role-to-group", "system:image-puller",
				"system:serviceaccounts:openshift-catalogd").Execute()
		//err = oc.AsAdmin().WithoutNamespace().Run("adm").
		//	Args("policy", "-n", "openshift-catalogd", "add-cluster-role-to-group", "system:image-puller",
		//		"system:serviceaccounts:openshift-catalogd").Execute()
		err = oc.AsAdmin().WithoutNamespace().Run("adm").
			Args("policy", "-n", "openshift-catalogd", "add-cluster-role-to-user", "system:image-puller",
				"-z", "catalogd-controller-manager").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		err = oc.AsAdmin().WithoutNamespace().Run("adm").
			Args("policy", "-n", "openshift-catalogd", "add-cluster-role-to-user", "system:image-pusher",
				"-z", "catalogd-controller-manager").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		err = oc.AsAdmin().WithoutNamespace().Run("adm").
			Args("policy", "-n", "openshift-catalogd", "add-cluster-role-to-user", "system:image-builder",
				"-z", "catalogd-controller-manager").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	g.AfterEach(func() {
		if g.CurrentSpecReport().Failed() {
			oc.AsAdmin().Run("get").Args("clustercatalog", "catalog-test")
			exutil.DumpPodLogsStartingWith("", oc)
		}
		oc.AdminConfigClient().ConfigV1().ImageTagMirrorSets().Delete(context.Background(), "catalog-test", metav1.DeleteOptions{})
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
		logs, err := br.Logs()
		o.Expect(err).NotTo(o.HaveOccurred())
		imageLocRegex := regexp.MustCompile(`image-registry\.openshift-image-registry\.svc:5000/[-a-z0-9]+/catalog-test`)
		imageLoc := imageLocRegex.FindString(logs)
		g.GinkgoWriter.Printf("IMAGE NAME: %q\n", imageLoc)

		//logs, err = br.Logs()
		//o.Expect(err).NotTo(o.HaveOccurred())
		//imageRefRegex := regexp.MustCompile(`image-registry\.openshift-image-registry\.svc:5000/[a=z0-9]+/catalog-test@sha256:[a-z0-9]+`)
		//imageRef := imageRefRegex.FindString(logs)
		//g.GinkgoWriter.Printf("IMAGE REF: %q\n", imageRef)

		//g.By("creating an ITMS")
		//itms := newImageTagMirrorSet(imageLoc)
		//_, err = oc.AdminConfigClient().ConfigV1().ImageTagMirrorSets().Create(context.Background(), itms, metav1.CreateOptions{})
		//o.Expect(err).NotTo(o.HaveOccurred())

		g.By("creating the ClusterCatalog")
		_ = applyClusterCatalog(oc, imageLoc+":latest", clusterCatalogFile)
		//cleanup := applyClusterCatalog(oc, imageLoc+":latest", clusterCatalogFile)
		//g.DeferCleanup(cleanup)

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
			if !meta.IsStatusConditionPresentAndEqual(conditions, "Progressing", metav1.ConditionTrue) {
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

func applyClusterCatalog(oc *exutil.CLI, ref, ccFile string) func() {
	ns := oc.Namespace()
	g.By(fmt.Sprintf("updating the namespace to: %q", ns))
	newCcFile := ccFile + "." + ns
	b, err := os.ReadFile(ccFile)
	o.Expect(err).NotTo(o.HaveOccurred())
	s := string(b)
	s = strings.ReplaceAll(s, "{REF}", ref)
	err = os.WriteFile(newCcFile, []byte(s), 0666)
	o.Expect(err).NotTo(o.HaveOccurred())

	g.By("applying the necessary resources")
	err = oc.AsAdmin().WithoutNamespace().Run("apply").Args("-f", newCcFile).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
	return func() {
		g.By("cleaning the necessary resources")
		oc.AsAdmin().WithoutNamespace().Run("delete").Args("-f", newCcFile).Execute()
	}
}
