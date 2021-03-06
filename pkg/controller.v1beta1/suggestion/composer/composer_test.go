package composer

import (
	"context"
	"encoding/json"
	"fmt"
	stdlog "log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/spf13/viper"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	apis "github.com/kubeflow/katib/pkg/apis/controller"
	experimentsv1beta1 "github.com/kubeflow/katib/pkg/apis/controller/experiments/v1beta1"
	suggestionsv1beta1 "github.com/kubeflow/katib/pkg/apis/controller/suggestions/v1beta1"
	"github.com/kubeflow/katib/pkg/controller.v1beta1/consts"
	"github.com/kubeflow/katib/pkg/util/v1beta1/katibconfig"
)

var (
	cfg     *rest.Config
	timeout = time.Second * 40

	suggestionName      = "test-suggestion"
	suggestionAlgorithm = "random"
	suggestionLabels    = map[string]string{
		"custom-label": "test",
	}
	suggestionAnnotations = map[string]string{
		"custom-annotation": "test",
	}

	deploymentLabels = map[string]string{
		"custom-label": "test",
		"deployment":   suggestionName + "-" + suggestionAlgorithm,
		"experiment":   suggestionName,
		"suggestion":   suggestionName,
	}

	podAnnotations = map[string]string{
		"custom-annotation":       "test",
		"sidecar.istio.io/inject": "false",
	}

	namespace       = "kubeflow"
	configMap       = "katib-config"
	serviceAccount  = "test-serviceaccount"
	image           = "test-image"
	imagePullPolicy = corev1.PullAlways

	cpu    = "2m"
	memory = "3Mi"
	disk   = "4Gi"

	refFlag bool = true

	storageClassName = consts.DefaultSuggestionStorageClassName
)

func TestMain(m *testing.M) {
	// Start test k8s server
	t := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "..", "manifests", "v1beta1", "katib-controller"),
		},
	}
	apis.AddToScheme(scheme.Scheme)

	var err error
	if cfg, err = t.Start(); err != nil {
		stdlog.Fatal(err)
	}

	code := m.Run()
	t.Stop()
	os.Exit(code)
}

func StartTestManager(mgr manager.Manager, g *gomega.GomegaWithT) (chan struct{}, *sync.WaitGroup) {
	stop := make(chan struct{})
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.Expect(mgr.Start(stop)).NotTo(gomega.HaveOccurred())
	}()
	return stop, wg
}

func TestDesiredDeployment(t *testing.T) {
	g := gomega.NewGomegaWithT(t)

	mgr, err := manager.New(cfg, manager.Options{})
	g.Expect(err).NotTo(gomega.HaveOccurred())

	stopMgr, mgrStopped := StartTestManager(mgr, g)
	defer func() {
		close(stopMgr)
		mgrStopped.Wait()
	}()

	c := mgr.GetClient()

	composer := New(mgr)

	tcs := []struct {
		suggestion         *suggestionsv1beta1.Suggestion
		configMap          *corev1.ConfigMap
		expectedDeployment *appsv1.Deployment
		err                bool
		testDescription    string
	}{
		{
			suggestion:      newFakeSuggestion(),
			configMap:       newFakeKatibConfig(newFakeSuggestionConfig()),
			err:             true,
			testDescription: "Set controller reference error",
		},
		{
			suggestion:         newFakeSuggestion(),
			configMap:          newFakeKatibConfig(newFakeSuggestionConfig()),
			expectedDeployment: newFakeDeployment(),
			err:                false,
			testDescription:    "Desired Deployment valid run",
		},
		{
			suggestion: newFakeSuggestion(),
			configMap: func() *corev1.ConfigMap {
				cm := newFakeKatibConfig(newFakeSuggestionConfig())
				cm.Data["suggestion"] = strings.ReplaceAll(cm.Data["suggestion"], string(imagePullPolicy), "invalid")
				return cm
			}(),
			expectedDeployment: func() *appsv1.Deployment {
				deploy := newFakeDeployment()
				deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
				return deploy
			}(),
			err:             false,
			testDescription: "Image Pull Policy set to default",
		},
		{
			suggestion: newFakeSuggestion(),
			configMap: func() *corev1.ConfigMap {
				cm := newFakeKatibConfig(newFakeSuggestionConfig())
				cm.Data["suggestion"] = strings.ReplaceAll(cm.Data["suggestion"], cpu, "invalid")
				return cm
			}(),
			err:             true,
			testDescription: "Get suggestion config error, invalid CPU limit",
		},
		{
			suggestion: newFakeSuggestion(),
			configMap: func() *corev1.ConfigMap {
				sc := newFakeSuggestionConfig()
				sc.VolumeMountPath = "/custom/container/path"
				cm := newFakeKatibConfig(sc)
				return cm
			}(),
			expectedDeployment: func() *appsv1.Deployment {
				deploy := newFakeDeployment()
				deploy.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath = "/custom/container/path"
				return deploy
			}(),
			err:             false,
			testDescription: "Suggestion container with custom volume mount path",
		},
	}

	viper.Set(consts.ConfigEnableGRPCProbeInSuggestion, true)

	for idx, tc := range tcs {
		// Create configMap with Katib config
		g.Expect(c.Create(context.TODO(), tc.configMap)).NotTo(gomega.HaveOccurred())

		// Wait that Config Map is created
		g.Eventually(func() error {
			return c.Get(context.TODO(), types.NamespacedName{Namespace: namespace, Name: configMap}, &corev1.ConfigMap{})
		}, timeout).ShouldNot(gomega.HaveOccurred())

		// Get deployment
		var actualDeployment *appsv1.Deployment
		var err error
		// For the first Test we run DesiredDeployment with empty Scheme to fail Set Controller Reference
		if idx == 0 {
			c := General{
				scheme: &runtime.Scheme{},
				Client: mgr.GetClient(),
			}
			actualDeployment, err = c.DesiredDeployment(tc.suggestion)
		} else {
			actualDeployment, err = composer.DesiredDeployment(tc.suggestion)
		}

		if !tc.err && err != nil {
			t.Errorf("Case: %v failed. Expected nil, got %v", tc.testDescription, err)
		} else if tc.err && err == nil {
			t.Errorf("Case: %v failed. Expected err, got nil", tc.testDescription)
		} else if !tc.err && !metaEqual(tc.expectedDeployment.ObjectMeta, actualDeployment.ObjectMeta) {
			t.Errorf("Case: %v failed. \nExpected deploy metadata %v\n Got %v", tc.testDescription, tc.expectedDeployment.ObjectMeta, actualDeployment.ObjectMeta)
		} else if !tc.err && !equality.Semantic.DeepEqual(tc.expectedDeployment.Spec, actualDeployment.Spec) {
			t.Errorf("Case: %v failed. \nExpected deploy spec %v\n Got %v", tc.testDescription, tc.expectedDeployment.Spec, actualDeployment.Spec)
		}

		// Delete configMap with Katib config
		g.Expect(c.Delete(context.TODO(), tc.configMap)).NotTo(gomega.HaveOccurred())

		// Wait that Config Map is deleted
		g.Eventually(func() bool {
			return errors.IsNotFound(
				c.Get(context.TODO(), types.NamespacedName{Namespace: namespace, Name: configMap}, &corev1.ConfigMap{}))
		}, timeout).Should(gomega.BeTrue())

	}
}

func TestDesiredService(t *testing.T) {

	g := gomega.NewGomegaWithT(t)

	mgr, err := manager.New(cfg, manager.Options{})
	g.Expect(err).NotTo(gomega.HaveOccurred())

	composer := New(mgr)

	expectedService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      suggestionName + "-" + suggestionAlgorithm,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "kubeflow.org/v1beta1",
					Kind:               "Suggestion",
					Name:               suggestionName,
					Controller:         &refFlag,
					BlockOwnerDeletion: &refFlag,
				},
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: deploymentLabels,
			Ports: []corev1.ServicePort{
				{
					Name: consts.DefaultSuggestionPortName,
					Port: consts.DefaultSuggestionPort,
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	tcs := []struct {
		suggestion      *suggestionsv1beta1.Suggestion
		expectedService *corev1.Service
		err             bool
		testDescription string
	}{
		{
			suggestion:      newFakeSuggestion(),
			err:             true,
			testDescription: "Set controller reference error",
		},
		{
			suggestion:      newFakeSuggestion(),
			expectedService: expectedService,
			err:             false,
			testDescription: "Desired Service valid run",
		},
	}

	for idx, tc := range tcs {

		// Get service
		var actualService *corev1.Service
		var err error
		// For the first Test we run DesiredService with empty Scheme to fail Set Controller Reference
		if idx == 0 {
			c := General{
				scheme: &runtime.Scheme{},
				Client: mgr.GetClient(),
			}
			actualService, err = c.DesiredService(tc.suggestion)
		} else {
			actualService, err = composer.DesiredService(tc.suggestion)
		}

		if !tc.err && err != nil {
			t.Errorf("Case: %v failed. Expected nil, got %v", tc.testDescription, err)
		} else if tc.err && err == nil {
			t.Errorf("Case: %v failed. Expected err, got nil", tc.testDescription)
		} else if !tc.err && !metaEqual(tc.expectedService.ObjectMeta, actualService.ObjectMeta) {
			t.Errorf("Case: %v failed. \nExpected service metadata %v\n Got %v", tc.testDescription, tc.expectedService.ObjectMeta, actualService.ObjectMeta)
		} else if !tc.err && !equality.Semantic.DeepEqual(tc.expectedService.Spec, actualService.Spec) {
			t.Errorf("Case: %v failed. \nExpected service spec %v\n Got %v", tc.testDescription, tc.expectedService.Spec, actualService.Spec)
		}
	}
}

func TestDesiredVolume(t *testing.T) {

	g := gomega.NewGomegaWithT(t)

	mgr, err := manager.New(cfg, manager.Options{})
	g.Expect(err).NotTo(gomega.HaveOccurred())

	stopMgr, mgrStopped := StartTestManager(mgr, g)
	defer func() {
		close(stopMgr)
		mgrStopped.Wait()
	}()

	c := mgr.GetClient()

	composer := New(mgr)

	tcs := []struct {
		suggestion      *suggestionsv1beta1.Suggestion
		configMap       *corev1.ConfigMap
		expectedPVC     *corev1.PersistentVolumeClaim
		expectedPV      *corev1.PersistentVolume
		err             bool
		testDescription string
	}{
		{
			suggestion:      newFakeSuggestion(),
			configMap:       newFakeKatibConfig(newFakeSuggestionConfig()),
			err:             true,
			testDescription: "Set controller reference error",
		},
		{
			suggestion:      newFakeSuggestion(),
			err:             true,
			testDescription: "Get suggestion config error, not found Katib config",
		},
		{
			suggestion:      newFakeSuggestion(),
			configMap:       newFakeKatibConfig(newFakeSuggestionConfig()),
			expectedPVC:     newFakePVC(),
			expectedPV:      newFakePV(),
			err:             false,
			testDescription: "Desired Volume valid run with default pvc and pv",
		},
		{
			suggestion: newFakeSuggestion(),
			configMap: func() *corev1.ConfigMap {
				sc := newFakeSuggestionConfig()
				storageClass := "custom-storage-class"
				volumeStorage, _ := resource.ParseQuantity("5Gi")

				sc.PersistentVolumeClaimSpec = corev1.PersistentVolumeClaimSpec{
					StorageClassName: &storageClass,
					AccessModes: []corev1.PersistentVolumeAccessMode{
						corev1.ReadWriteOnce,
						corev1.ReadOnlyMany,
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: volumeStorage,
						},
					},
				}
				cm := newFakeKatibConfig(sc)
				return cm
			}(),
			expectedPVC: func() *corev1.PersistentVolumeClaim {
				pvc := newFakePVC()
				storageClass := "custom-storage-class"
				volumeStorage, _ := resource.ParseQuantity("5Gi")

				pvc.Spec.StorageClassName = &storageClass
				pvc.Spec.AccessModes = append(pvc.Spec.AccessModes, corev1.ReadOnlyMany)
				pvc.Spec.Resources.Requests[corev1.ResourceStorage] = volumeStorage
				return pvc
			}(),
			expectedPV:      nil,
			err:             false,
			testDescription: "Custom PVC with not default storage class",
		},
		{
			suggestion: newFakeSuggestion(),
			configMap: func() *corev1.ConfigMap {
				sc := newFakeSuggestionConfig()
				mode := corev1.PersistentVolumeFilesystem
				accessModes := []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteOnce,
					corev1.ReadOnlyMany,
				}
				volumeStorage, _ := resource.ParseQuantity("10Gi")

				sc.PersistentVolumeClaimSpec = corev1.PersistentVolumeClaimSpec{
					VolumeMode:  &mode,
					AccessModes: accessModes,
				}

				sc.PersistentVolumeSpec = corev1.PersistentVolumeSpec{
					VolumeMode:  &mode,
					AccessModes: accessModes,
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						GCEPersistentDisk: &corev1.GCEPersistentDiskVolumeSource{
							PDName: "pd-name",
							FSType: "fs-type",
						},
					},
					Capacity: corev1.ResourceList{
						corev1.ResourceStorage: volumeStorage,
					},
				}
				cm := newFakeKatibConfig(sc)
				return cm
			}(),
			expectedPVC: func() *corev1.PersistentVolumeClaim {
				pvc := newFakePVC()
				mode := corev1.PersistentVolumeFilesystem

				pvc.Spec.VolumeMode = &mode
				pvc.Spec.AccessModes = append(pvc.Spec.AccessModes, corev1.ReadOnlyMany)
				return pvc
			}(),
			expectedPV: func() *corev1.PersistentVolume {
				pv := newFakePV()
				mode := corev1.PersistentVolumeFilesystem
				volumeStorage, _ := resource.ParseQuantity("10Gi")

				pv.Spec.VolumeMode = &mode
				pv.Spec.AccessModes = append(pv.Spec.AccessModes, corev1.ReadOnlyMany)
				pv.Spec.PersistentVolumeSource = corev1.PersistentVolumeSource{
					GCEPersistentDisk: &corev1.GCEPersistentDiskVolumeSource{
						PDName: "pd-name",
						FSType: "fs-type",
					},
				}
				pv.Spec.Capacity = corev1.ResourceList{
					corev1.ResourceStorage: volumeStorage,
				}
				return pv
			}(),
			err:             false,
			testDescription: "Custom PVC and PV with default storage class",
		},
	}

	for idx, tc := range tcs {

		if tc.configMap != nil {
			// Expect that ConfigMap is created
			g.Eventually(func() error {
				// Create ConfigMap with Katib config
				c.Create(context.TODO(), tc.configMap)
				return c.Get(context.TODO(), types.NamespacedName{Namespace: namespace, Name: configMap}, &corev1.ConfigMap{})
			}, timeout).ShouldNot(gomega.HaveOccurred())
		}

		// Get PVC and PV
		var actualPVC *corev1.PersistentVolumeClaim
		var actualPV *corev1.PersistentVolume
		var err error
		// For the first Test we run DesiredVolume with empty Scheme to fail Set Controller Reference
		if idx == 0 {
			c := General{
				scheme: &runtime.Scheme{},
				Client: mgr.GetClient(),
			}
			actualPVC, actualPV, err = c.DesiredVolume(tc.suggestion)
		} else {
			actualPVC, actualPV, err = composer.DesiredVolume(tc.suggestion)
		}

		if !tc.err && err != nil {
			t.Errorf("Case: %v failed. Expected nil, got %v", tc.testDescription, err)

		} else if tc.err && err == nil {
			t.Errorf("Case: %v failed. Expected err, got nil", tc.testDescription)

		} else if !tc.err && ((tc.expectedPV == nil && actualPV != nil) || (tc.expectedPV != nil && actualPV == nil)) {
			t.Errorf("Case: %v failed. \nExpected PV: %v\n Got %v", tc.testDescription, tc.expectedPV, actualPV)

		} else if !tc.err && (!metaEqual(tc.expectedPVC.ObjectMeta, actualPVC.ObjectMeta) ||
			(tc.expectedPV != nil && !metaEqual(tc.expectedPV.ObjectMeta, actualPV.ObjectMeta))) {
			t.Errorf("Case: %v failed. \nExpected PVC metadata %v\n Got %v.\nExpected PV metadata %v\n Got %v",
				tc.testDescription, tc.expectedPVC.ObjectMeta, actualPVC.ObjectMeta, tc.expectedPV.ObjectMeta, actualPV.ObjectMeta)

		} else if !tc.err && (!equality.Semantic.DeepEqual(tc.expectedPVC.Spec, actualPVC.Spec) ||
			(tc.expectedPV != nil && !equality.Semantic.DeepEqual(tc.expectedPV.Spec, actualPV.Spec))) {
			t.Errorf("Case: %v failed. \nExpected PVC spec %v\n Got %v.\nExpected PV spec %v\n Got %v",
				tc.testDescription, tc.expectedPVC.Spec, actualPVC.Spec, tc.expectedPV, actualPV)

		}

		if tc.configMap != nil {
			// Expect that ConfigMap is deleted
			g.Eventually(func() bool {
				// Delete ConfigMap with Katib config
				c.Delete(context.TODO(), tc.configMap)
				return errors.IsNotFound(
					c.Get(context.TODO(), types.NamespacedName{Namespace: namespace, Name: configMap}, &corev1.ConfigMap{}))
			}, timeout).Should(gomega.BeTrue())
		}

	}
}

func metaEqual(expected, actual metav1.ObjectMeta) bool {
	return expected.Name == actual.Name &&
		expected.Namespace == actual.Namespace &&
		reflect.DeepEqual(expected.Labels, actual.Labels) &&
		reflect.DeepEqual(expected.Annotations, actual.Annotations) &&
		len(actual.OwnerReferences) > 0 &&
		expected.OwnerReferences[0].APIVersion == actual.OwnerReferences[0].APIVersion &&
		expected.OwnerReferences[0].Kind == actual.OwnerReferences[0].Kind &&
		expected.OwnerReferences[0].Name == actual.OwnerReferences[0].Name &&
		*expected.OwnerReferences[0].Controller == *actual.OwnerReferences[0].Controller &&
		*expected.OwnerReferences[0].BlockOwnerDeletion == *actual.OwnerReferences[0].BlockOwnerDeletion
}

func newFakeSuggestionConfig() katibconfig.SuggestionConfig {
	cpuQ, _ := resource.ParseQuantity(cpu)
	memoryQ, _ := resource.ParseQuantity(memory)
	diskQ, _ := resource.ParseQuantity(disk)

	return katibconfig.SuggestionConfig{
		Image:           image,
		ImagePullPolicy: imagePullPolicy,
		Resource: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:              cpuQ,
				corev1.ResourceMemory:           memoryQ,
				corev1.ResourceEphemeralStorage: diskQ,
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:              cpuQ,
				corev1.ResourceMemory:           memoryQ,
				corev1.ResourceEphemeralStorage: diskQ,
			},
		},
		ServiceAccountName: serviceAccount,
	}
}

func newFakeKatibConfig(suggestionConfig katibconfig.SuggestionConfig) *corev1.ConfigMap {

	jsonConfig := map[string]katibconfig.SuggestionConfig{
		"random": suggestionConfig,
	}

	b, _ := json.Marshal(jsonConfig)

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMap,
			Namespace: namespace,
		},
		Data: map[string]string{
			"suggestion": string(b),
		},
	}
}

func newFakeSuggestion() *suggestionsv1beta1.Suggestion {
	return &suggestionsv1beta1.Suggestion{
		ObjectMeta: metav1.ObjectMeta{
			Name:        suggestionName,
			Namespace:   namespace,
			Labels:      suggestionLabels,
			Annotations: suggestionAnnotations,
		},
		Spec: suggestionsv1beta1.SuggestionSpec{
			Requests:      1,
			AlgorithmName: suggestionAlgorithm,
			ResumePolicy:  experimentsv1beta1.FromVolume,
		},
	}
}

func newFakeDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        suggestionName + "-" + suggestionAlgorithm,
			Namespace:   namespace,
			Labels:      suggestionLabels,
			Annotations: suggestionAnnotations,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "kubeflow.org/v1beta1",
					Kind:               "Suggestion",
					Name:               suggestionName,
					Controller:         &refFlag,
					BlockOwnerDeletion: &refFlag,
				},
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: deploymentLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      deploymentLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					Containers:         newFakeContainers(),
					ServiceAccountName: serviceAccount,
					Volumes: []corev1.Volume{
						{
							Name: consts.ContainerSuggestionVolumeName,
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: suggestionName + "-" + suggestionAlgorithm,
								},
							},
						},
					},
				},
			},
		},
	}
}

func newFakeContainers() []corev1.Container {

	cpuQ, _ := resource.ParseQuantity(cpu)
	memoryQ, _ := resource.ParseQuantity(memory)
	diskQ, _ := resource.ParseQuantity(disk)

	return []corev1.Container{
		{
			Name:            consts.ContainerSuggestion,
			Image:           image,
			ImagePullPolicy: corev1.PullAlways,
			Ports: []corev1.ContainerPort{
				{
					Name:          consts.DefaultSuggestionPortName,
					ContainerPort: consts.DefaultSuggestionPort,
				},
			},
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:              cpuQ,
					corev1.ResourceMemory:           memoryQ,
					corev1.ResourceEphemeralStorage: diskQ,
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:              cpuQ,
					corev1.ResourceMemory:           memoryQ,
					corev1.ResourceEphemeralStorage: diskQ,
				},
			},
			ReadinessProbe: &corev1.Probe{
				Handler: corev1.Handler{
					Exec: &corev1.ExecAction{
						Command: []string{
							defaultGRPCHealthCheckProbe,
							fmt.Sprintf("-addr=:%d", consts.DefaultSuggestionPort),
							fmt.Sprintf("-service=%s", consts.DefaultGRPCService),
						},
					},
				},
				InitialDelaySeconds: defaultInitialDelaySeconds,
				PeriodSeconds:       defaultPeriodForReady,
			},
			LivenessProbe: &corev1.Probe{
				Handler: corev1.Handler{
					Exec: &corev1.ExecAction{
						Command: []string{
							defaultGRPCHealthCheckProbe,
							fmt.Sprintf("-addr=:%d", consts.DefaultSuggestionPort),
							fmt.Sprintf("-service=%s", consts.DefaultGRPCService),
						},
					},
				},
				InitialDelaySeconds: defaultInitialDelaySeconds,
				PeriodSeconds:       defaultPeriodForLive,
				FailureThreshold:    defaultFailureThreshold,
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      consts.ContainerSuggestionVolumeName,
					MountPath: consts.DefaultContainerSuggestionVolumeMountPath,
				},
			},
		},
	}
}

func newFakePVC() *corev1.PersistentVolumeClaim {

	volumeStorage, _ := resource.ParseQuantity(consts.DefaultSuggestionVolumeStorage)

	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      suggestionName + "-" + suggestionAlgorithm,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "kubeflow.org/v1beta1",
					Kind:               "Suggestion",
					Name:               suggestionName,
					Controller:         &refFlag,
					BlockOwnerDeletion: &refFlag,
				},
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &storageClassName,
			AccessModes: []corev1.PersistentVolumeAccessMode{
				consts.DefaultSuggestionVolumeAccessMode,
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: volumeStorage,
				},
			},
		},
	}
}

func newFakePV() *corev1.PersistentVolume {
	pvName := suggestionName + "-" + suggestionAlgorithm + "-" + namespace
	volumeStorage, _ := resource.ParseQuantity(consts.DefaultSuggestionVolumeStorage)

	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
			Labels: map[string]string{
				"type": "local",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "kubeflow.org/v1beta1",
					Kind:               "Suggestion",
					Name:               suggestionName,
					Controller:         &refFlag,
					BlockOwnerDeletion: &refFlag,
				},
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: storageClassName,
			AccessModes: []corev1.PersistentVolumeAccessMode{
				consts.DefaultSuggestionVolumeAccessMode,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: consts.DefaultSuggestionVolumeLocalPathPrefix + pvName,
				},
			},
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: volumeStorage,
			},
		},
	}
}
