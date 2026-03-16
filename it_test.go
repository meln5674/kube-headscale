package main_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	gomegatypes "github.com/onsi/gomega/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	k8s "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	hsv1 "github.com/juanfont/headscale/gen/go/headscale/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestKubeHeadscale(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "KubeHeadscale Suite")
}

var _ = SynchronizedBeforeSuite(func() []byte {
	var fs flag.FlagSet
	klog.InitFlags(&fs)
	err := fs.Set("v", "3")
	Expect(err).ToNot(HaveOccurred())
	klog.SetOutput(GinkgoWriter)
	log.SetOutput(GinkgoWriter)

	startEnv(true)

	stBytes, err := json.Marshal(&suiteState{
		KubeconfigPath: kubeconfigPath,
		DebugKeyPath:   debugKeyPath,
	})
	Expect(err).ToNot(HaveOccurred())
	return stBytes
}, func(stBytes []byte) {

	var st suiteState
	Expect(json.Unmarshal(stBytes, &st))
	kubeconfigPath, debugKeyPath = st.KubeconfigPath, st.DebugKeyPath

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	Expect(err).ToNot(HaveOccurred())

	client, err = k8s.New(cfg, k8s.Options{})
	Expect(err).ToNot(HaveOccurred())
})

func startEnv(cleanup bool) {
	testEnv = &envtest.Environment{
		// ControlPlane: envtest.ControlPlane{
		// 	APIServer: &envtest.APIServer{
		// 		Out: os.Stdout,
		// 		Err: os.Stderr,
		// 	},
		// 	Etcd: &envtest.Etcd{
		// 		Out: os.Stdout,
		// 		Err: os.Stderr,
		// 	},
		// },
	}
	_, err := testEnv.Start()
	Expect(err).ToNot(HaveOccurred())
	if cleanup {
		DeferCleanup(testEnv.Stop)
	}

	tmp := GinkgoT().TempDir()

	kubeconfigPath = filepath.Join(tmp, "kubeconfig")
	Expect(os.WriteFile(kubeconfigPath, testEnv.KubeConfig, 0o600)).To(Succeed())

	debugKeyPath = filepath.Join(tmp, "debugkey")
	debugKeyBytes := make([]byte, 16)
	_, err = rand.Read(debugKeyBytes)
	Expect(err).ToNot(HaveOccurred())
	Expect(os.WriteFile(debugKeyPath, []byte(hex.EncodeToString(debugKeyBytes)), 0o600)).To(Succeed())
}

type suiteState struct {
	KubeconfigPath, DebugKeyPath string
}

var (
	testEnv                      *envtest.Environment
	client                       k8s.Client
	kubeconfigPath, debugKeyPath string
)

var _ = Describe("kube-headscale", func() {
	var t testCtx
	BeforeEach(func(ctx context.Context) {
		t = startTest(ctx)
	})

	It("should add a policy from a configmap with a single key", func(ctx context.Context) {
		dev1ID := t.mkUser(ctx, "dev1")
		dev1Key := t.mkPreAuthKey(ctx, dev1ID)
		t.tailscaleLoginPreAuth(ctx, dev1Key)
		t.mkUser(ctx, "dev2")
		t.mkUser(ctx, "admin1")
		policy := jobj{
			"groups": jobj{
				"group:dev":   jarr{"dev1@", "dev2@"},
				"group:admin": jarr{"admin1@"},
			},
			"tagOwners": jobj{
				"tag:prod-app-servers": jarr{"group:admin"},
				"tag:dev-databases":    jarr{"group:admin", "group:dev"},
			},
			"hosts": jobj{
				"postgresql.internal": "10.20.0.2/32",
				"webservers.internal": "10.20.10.1/29",
			},
			"acls": jarr{
				jobj{
					"action": "accept",
					"src":    jarr{"group:admin"},
					"proto":  "tcp",
					"dst": jarr{
						"tag:prod-app-servers:22",
						"tag:dev-databases:22",
					},
				},
				jobj{
					"action": "accept",
					"src":    jarr{"group:dev"},
					"dst": jarr{
						"tag:dev-databases:*",
						"tag:prod-app-servers:80,443",
					},
				},
			},
			"ssh": jarr{
				jobj{
					"action": "accept",
					"src":    jarr{"autogroup:member"},
					"dst":    jarr{"autogroup:self"},
					"users":  jarr{"autogroup:nonroot"},
				},
			},
		}

		t.mkPolicyCM(ctx, "policy.json", formatJObj(policy))

		t.expectPolicy(ctx, HaveKeyWithValue("hosts", policy["hosts"]))
	})
	It("should add a policy from a configmap with multiple keys", func(ctx context.Context) {
		t.mkUser(ctx, "dev1")
		t.mkUser(ctx, "dev2")
		t.mkUser(ctx, "admin1")
		policyA := jobj{
			"groups": jobj{
				"group:dev": jarr{"dev1@", "dev2@"},
			},
			"tagOwners": jobj{
				"tag:prod-app-servers": jarr{"group:admin"},
			},
			"hosts": jobj{
				"postgresql.internal": "10.20.0.2/32",
			},
			"acls": jarr{
				jobj{
					"action": "accept",
					"src":    jarr{"group:admin"},
					"proto":  "tcp",
					"dst": jarr{
						"tag:prod-app-servers:22",
						"tag:dev-databases:22",
					},
				},
			},

			"ssh": jarr{
				jobj{
					"action": "accept",
					"src":    jarr{"autogroup:member"},
					"dst":    jarr{"autogroup:self"},
					"users":  jarr{"autogroup:nonroot"},
				},
			},
		}
		policyB := jobj{
			"groups": jobj{
				"group:admin": jarr{"admin1@"},
			},
			"tagOwners": jobj{
				"tag:dev-databases": jarr{"group:admin", "group:dev"},
			},
			"hosts": jobj{
				"webservers.internal": "10.20.10.1/29",
			},
			"acls": jarr{
				jobj{
					"action": "accept",
					"src":    jarr{"group:dev"},
					"dst": jarr{
						"tag:dev-databases:*",
						"tag:prod-app-servers:80,443",
					},
				},
			},
		}

		t.mkPolicyCM(ctx, "policyA.json", formatJObj(policyA), "policyB.json", formatJObj(policyB))

		t.expectPolicy(ctx, And(
			HaveKeyWithValue("hosts", HaveKeyWithValue("postgresql.internal", policyA["hosts"].(jobj)["postgresql.internal"])),
			HaveKeyWithValue("hosts", HaveKeyWithValue("webservers.internal", policyB["hosts"].(jobj)["webservers.internal"])),
		))
	})

	It("should remove a policy when a configmap key is removed", func(ctx context.Context) {
		t.mkUser(ctx, "dev1")
		t.mkUser(ctx, "dev2")
		t.mkUser(ctx, "admin1")
		policyA := jobj{
			"groups": jobj{
				"group:dev":   jarr{"dev1@", "dev2@"},
				"group:admin": jarr{"admin1@"},
			},
			"hosts": jobj{
				"postgresql.internal": "10.20.0.2/32",
			},
			"tagOwners": jobj{
				"tag:dev-databases":    jarr{"group:admin", "group:dev"},
				"tag:prod-app-servers": jarr{"group:admin"},
			},
			"acls": jarr{
				jobj{
					"action": "accept",
					"src":    jarr{"group:dev"},
					"dst": jarr{
						"tag:dev-databases:*",
						"tag:prod-app-servers:80,443",
					},
				},
			},
			"ssh": jarr{
				jobj{
					"action": "accept",
					"src":    jarr{"autogroup:member"},
					"dst":    jarr{"autogroup:self"},
					"users":  jarr{"autogroup:nonroot"},
				},
			},
		}
		policyB := jobj{
			"hosts": jobj{
				"webservers.internal": "10.20.10.1/29",
			},
			"acls": jarr{
				jobj{
					"action": "accept",
					"src":    jarr{"group:admin"},
					"proto":  "tcp",
					"dst": jarr{
						"tag:prod-app-servers:22",
						"tag:dev-databases:22",
					},
				},
			},
		}

		cm := t.mkPolicyCM(ctx, "policyA.json", formatJObj(policyA), "policyB.json", formatJObj(policyB))

		t.expectPolicy(ctx, And(
			HaveKeyWithValue("hosts", HaveKeyWithValue("postgresql.internal", policyA["hosts"].(jobj)["postgresql.internal"])),
			HaveKeyWithValue("hosts", HaveKeyWithValue("webservers.internal", policyB["hosts"].(jobj)["webservers.internal"])),
		))

		Expect(client.Get(ctx, k8s.ObjectKeyFromObject(&cm), &cm)).To(Succeed())
		delete(cm.Data, "policyB.json")
		Expect(client.Update(ctx, &cm)).To(Succeed())

		t.expectPolicy(ctx, And(
			HaveKeyWithValue("hosts", HaveKeyWithValue("postgresql.internal", policyA["hosts"].(jobj)["postgresql.internal"])),
			HaveKeyWithValue("hosts", Not(HaveKey("webservers.internal"))),
		))
	})

	It("should add a policy from multiple configmaps with a single key each", func(ctx context.Context) {
		t.mkUser(ctx, "dev1")
		t.mkUser(ctx, "dev2")
		t.mkUser(ctx, "admin1")
		policyA := jobj{
			"groups": jobj{
				"group:dev": jarr{"dev1@", "dev2@"},
			},
			"tagOwners": jobj{
				"tag:prod-app-servers": jarr{"group:admin"},
			},
			"hosts": jobj{
				"postgresql.internal": "10.20.0.2/32",
			},
			"acls": jarr{
				jobj{
					"action": "accept",
					"src":    jarr{"group:admin"},
					"proto":  "tcp",
					"dst": jarr{
						"tag:prod-app-servers:22",
						"tag:dev-databases:22",
					},
				},
			},

			"ssh": jarr{
				jobj{
					"action": "accept",
					"src":    jarr{"autogroup:member"},
					"dst":    jarr{"autogroup:self"},
					"users":  jarr{"autogroup:nonroot"},
				},
			},
		}
		policyB := jobj{
			"groups": jobj{
				"group:admin": jarr{"admin1@"},
			},
			"tagOwners": jobj{
				"tag:dev-databases": jarr{"group:admin", "group:dev"},
			},
			"hosts": jobj{
				"webservers.internal": "10.20.10.1/29",
			},
			"acls": jarr{
				jobj{
					"action": "accept",
					"src":    jarr{"group:dev"},
					"dst": jarr{
						"tag:dev-databases:*",
						"tag:prod-app-servers:80,443",
					},
				},
			},
		}

		t.mkPolicyCM(ctx, "policyA.json", formatJObj(policyA))
		t.mkPolicyCM(ctx, "policyB.json", formatJObj(policyB))

		t.expectPolicy(ctx, And(
			HaveKeyWithValue("hosts", HaveKeyWithValue("postgresql.internal", policyA["hosts"].(jobj)["postgresql.internal"])),
			HaveKeyWithValue("hosts", HaveKeyWithValue("webservers.internal", policyB["hosts"].(jobj)["webservers.internal"])),
		))
	})

	It("should remove a policy when a configmap is deleted", func(ctx context.Context) {
		t.mkUser(ctx, "dev1")
		t.mkUser(ctx, "dev2")
		t.mkUser(ctx, "admin1")
		policyA := jobj{
			"groups": jobj{
				"group:dev":   jarr{"dev1@", "dev2@"},
				"group:admin": jarr{"admin1@"},
			},
			"hosts": jobj{
				"postgresql.internal": "10.20.0.2/32",
			},
			"tagOwners": jobj{
				"tag:dev-databases":    jarr{"group:admin", "group:dev"},
				"tag:prod-app-servers": jarr{"group:admin"},
			},
			"acls": jarr{
				jobj{
					"action": "accept",
					"src":    jarr{"group:dev"},
					"dst": jarr{
						"tag:dev-databases:*",
						"tag:prod-app-servers:80,443",
					},
				},
			},
			"ssh": jarr{
				jobj{
					"action": "accept",
					"src":    jarr{"autogroup:member"},
					"dst":    jarr{"autogroup:self"},
					"users":  jarr{"autogroup:nonroot"},
				},
			},
		}
		policyB := jobj{
			"hosts": jobj{
				"webservers.internal": "10.20.10.1/29",
			},
			"acls": jarr{
				jobj{
					"action": "accept",
					"src":    jarr{"group:admin"},
					"proto":  "tcp",
					"dst": jarr{
						"tag:prod-app-servers:22",
						"tag:dev-databases:22",
					},
				},
			},
		}

		t.mkPolicyCM(ctx, "policyA.json", formatJObj(policyA))
		cm := t.mkPolicyCM(ctx, "policyB.json", formatJObj(policyB))

		t.expectPolicy(ctx, And(
			HaveKeyWithValue("hosts", HaveKeyWithValue("postgresql.internal", policyA["hosts"].(jobj)["postgresql.internal"])),
			HaveKeyWithValue("hosts", HaveKeyWithValue("webservers.internal", policyB["hosts"].(jobj)["webservers.internal"])),
		))

		Expect(client.Delete(ctx, &cm, k8s.PropagationPolicy("Foreground"))).To(Succeed())
		t.expectPolicy(ctx, And(
			HaveKeyWithValue("hosts", HaveKeyWithValue("postgresql.internal", policyA["hosts"].(jobj)["postgresql.internal"])),
			HaveKeyWithValue("hosts", Not(HaveKey("webservers.internal"))),
		))
	})

	It("should add a derpmap from a configmap with a single key", func(ctx context.Context) {
		userID := t.mkUser(ctx, "user")
		userKey := t.mkPreAuthKey(ctx, userID)
		t.tailscaleLoginPreAuth(ctx, userKey)
		t.mkDERPMapCM(ctx, "derpmap.json", formatJIObj(jiobj{
			901: jobj{
				"regionid":   901,
				"regioncode": "test",
				"regionname": "Test Region",
				"nodes": jarr{jobj{
					"name":     "testnode",
					"regionid": 901,
					"hostname": "testnode.example.com",
					"ipv4":     "192.0.2.1",
				}},
			},
		}))

		t.expectDerpMap(ctx, HaveKeyWithValue("Regions", HaveKeyWithValue("901", jobj{
			"RegionID":   float64(901),
			"RegionCode": "test",
			"RegionName": "Test Region",
			"Nodes": jarr{
				jobj{
					"Name":     "testnode",
					"RegionID": float64(901),
					"HostName": "testnode.example.com",
					"IPv4":     "192.0.2.1",
				},
			},
		})))
	})

	It("should add a derpmap from a configmap with multiple keys", func(ctx context.Context) {
		userID := t.mkUser(ctx, "user")
		userKey := t.mkPreAuthKey(ctx, userID)
		t.tailscaleLoginPreAuth(ctx, userKey)
		t.mkDERPMapCM(ctx,
			"derpmap1.json", formatJIObj(jiobj{
				901: jobj{
					"regionid":   901,
					"regioncode": "test1",
					"regionname": "Test Region 1",
					"nodes": jarr{jobj{
						"name":     "testnode1",
						"regionid": 901,
						"hostname": "testnode1.example.com",
						"ipv4":     "192.0.2.1",
					}},
				},
			}),

			"derpmap2.json", formatJIObj(jiobj{
				902: jobj{
					"regionid":   902,
					"regioncode": "test2",
					"regionname": "Test Region 2",
					"nodes": jarr{jobj{
						"name":     "testnode2",
						"regionid": 902,
						"hostname": "testnode2.example.com",
						"ipv4":     "192.0.2.2",
					}},
				},
			}),
		)

		t.expectDerpMap(ctx, HaveKeyWithValue("Regions",
			And(
				HaveKeyWithValue("901", jobj{
					"RegionID":   float64(901),
					"RegionCode": "test1",
					"RegionName": "Test Region 1",
					"Nodes": jarr{
						jobj{
							"Name":     "testnode1",
							"RegionID": float64(901),
							"HostName": "testnode1.example.com",
							"IPv4":     "192.0.2.1",
						},
					},
				}),
				HaveKeyWithValue("902", jobj{
					"RegionID":   float64(902),
					"RegionCode": "test2",
					"RegionName": "Test Region 2",
					"Nodes": jarr{
						jobj{
							"Name":     "testnode2",
							"RegionID": float64(902),
							"HostName": "testnode2.example.com",
							"IPv4":     "192.0.2.2",
						},
					},
				}),
			),
		))
	})

	It("should add a derpmap from multiple configmaps with a single key each", func(ctx context.Context) {
		userID := t.mkUser(ctx, "user")
		userKey := t.mkPreAuthKey(ctx, userID)
		t.tailscaleLoginPreAuth(ctx, userKey)
		t.mkDERPMapCM(ctx,
			"derpmap1.json", formatJIObj(jiobj{
				901: jobj{
					"regionid":   901,
					"regioncode": "test1",
					"regionname": "Test Region 1",
					"nodes": jarr{jobj{
						"name":     "testnode1",
						"regionid": 901,
						"hostname": "testnode1.example.com",
						"ipv4":     "192.0.2.1",
					}},
				},
			}),
		)

		t.mkDERPMapCM(ctx,
			"derpmap2.json", formatJIObj(jiobj{
				902: jobj{
					"regionid":   902,
					"regioncode": "test2",
					"regionname": "Test Region 2",
					"nodes": jarr{jobj{
						"name":     "testnode2",
						"regionid": 902,
						"hostname": "testnode2.example.com",
						"ipv4":     "192.0.2.2",
					}},
				},
			}),
		)

		t.expectDerpMap(ctx, HaveKeyWithValue("Regions",
			And(
				HaveKeyWithValue("901", jobj{
					"RegionID":   float64(901),
					"RegionCode": "test1",
					"RegionName": "Test Region 1",
					"Nodes": jarr{
						jobj{
							"Name":     "testnode1",
							"RegionID": float64(901),
							"HostName": "testnode1.example.com",
							"IPv4":     "192.0.2.1",
						},
					},
				}),
				HaveKeyWithValue("902", jobj{
					"RegionID":   float64(902),
					"RegionCode": "test2",
					"RegionName": "Test Region 2",
					"Nodes": jarr{
						jobj{
							"Name":     "testnode2",
							"RegionID": float64(902),
							"HostName": "testnode2.example.com",
							"IPv4":     "192.0.2.2",
						},
					},
				}),
			),
		))
	})

	It("should generate a preauthkey", func(ctx context.Context) {
		t.mkUser(ctx, "user")
		secretName := t.mkPreAuthKeySecret(ctx, "user")
		userKey := t.waitForSecretKey(ctx, secretName, "preauthkey", "5s")
		t.tailscaleLoginPreAuth(ctx, userKey)
	})

	It("should generate an apikey with a default expiration", func(ctx context.Context) {
		secretName := t.mkApiKeySecret(ctx)
		apikey := t.waitForSecretKey(ctx, secretName, "apikey")

		hs := t.connectToHeadscaleGRPC(apikey)
		Consistently(func() error {
			_, err := hs.ListApiKeys(ctx, &hsv1.ListApiKeysRequest{})
			return err
		}, "5s", "1s").Should(Succeed())
	})

	It("should generate an apikey with an explicit expiration and not rotate it", func(ctx context.Context) {
		secretName := t.mkApiKeySecretExpiresAt(ctx, time.Now().Add(5*time.Second))
		apikey := t.waitForSecretKey(ctx, secretName, "apikey")

		hs := t.connectToHeadscaleGRPC(apikey)
		Consistently(func() error {
			_, err := hs.ListApiKeys(ctx, &hsv1.ListApiKeysRequest{})
			return err
		}, "3s", "1s").Should(Succeed())

		Eventually(func() error {
			_, err := hs.ListApiKeys(ctx, &hsv1.ListApiKeysRequest{})
			return err
		}, "3s", "1s").ShouldNot(Succeed())

		apikey2 := t.waitForSecretKey(ctx, secretName, "apikey")
		Expect(apikey2).To(Equal(apikey))
	})

	It("should generate an apikey with a lifetime and rotate it when it expires", func(ctx context.Context) {
		secretName := t.mkApiKeySecretExpiresAfter(ctx, 5*time.Second)
		apikey := t.waitForSecretKey(ctx, secretName, "apikey")

		hs := t.connectToHeadscaleGRPC(apikey)
		_, err := hs.ListApiKeys(ctx, &hsv1.ListApiKeysRequest{})
		Expect(err).ToNot(HaveOccurred())

		Consistently(func() error {
			_, err := hs.ListApiKeys(ctx, &hsv1.ListApiKeysRequest{})
			return err
		}, "3s", "1s").Should(Succeed())

		Eventually(func() error {
			_, err := hs.ListApiKeys(ctx, &hsv1.ListApiKeysRequest{})
			return err
		}, "3s", "1s").ShouldNot(Succeed())

		apikey2 := t.waitForSecretKeyChange(ctx, secretName, "apikey", apikey)

		hs = t.connectToHeadscaleGRPC(apikey2)
		Consistently(func() error {
			_, err := hs.ListApiKeys(ctx, &hsv1.ListApiKeysRequest{})
			return err
		}, "3s", "1s").Should(Succeed())
	})

})

type tokenAuth struct {
	token string
}

// Return value is mapped to request headers.
func (t tokenAuth) GetRequestMetadata(
	ctx context.Context,
	in ...string,
) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + t.token,
	}, nil
}

func (tokenAuth) RequireTransportSecurity() bool {
	return false
}

func (t *testCtx) waitForSecretKey(ctx context.Context, name, key string, eventualArgs ...any) string {
	return t.waitForSecretKeyChange(ctx, name, key, "", eventualArgs...)
}

func (t *testCtx) waitForSecretKeyChange(ctx context.Context, name, key string, old string, eventualArgs ...any) string {
	var v string
	Eventually(func() string {
		var s corev1.Secret
		Expect(client.Get(ctx, k8s.ObjectKey{Namespace: t.ns, Name: name}, &s)).To(Succeed())
		v = string(s.Data[key])
		return v
	}, eventualArgs...).ShouldNot(Equal(old))
	return v
}

type jobj = map[string]any

func formatJObj(j jobj) string {
	out, err := json.Marshal(j)
	Expect(err).ToNot(HaveOccurred())
	return string(out)
}

func parseJObj(s string) jobj {
	var j jobj
	Expect(json.Unmarshal([]byte(s), &j)).ToNot(HaveOccurred())
	return j
}

type jiobj = map[int]any

func formatJIObj(j jiobj) string {
	out, err := json.Marshal(j)
	Expect(err).ToNot(HaveOccurred())
	return string(out)
}

type jarr = []any

type testCtx struct {
	ctx                                  context.Context
	tmp                                  string
	ns                                   string
	hsSocketPath, dnsPath                string
	hsServerAddr, hsGrpcAddr, hsDerpAddr string
	sidecarAddr                          string
	hs                                   hsv1.HeadscaleServiceClient
	tsSocketPath                         string
}

func startTest(ctx context.Context) (t testCtx) {
	By("starting test")
	t.tmp = GinkgoT().TempDir()

	var cancel func()
	t.ctx, cancel = context.WithCancel(context.Background())
	DeferCleanup(cancel)

	t.mkNamespace(ctx)

	t.hsSocketPath = filepath.Join(t.tmp, "headscale.sock")
	t.dnsPath = filepath.Join(t.tmp, "dns.json")

	addrs := pickRandomPorts("127.0.0.1", 4)
	t.sidecarAddr = addrs[0]
	t.hsServerAddr = addrs[1]
	t.hsGrpcAddr = addrs[2]
	t.hsDerpAddr = addrs[3]

	t.startSidecar(ctx)

	t.startHeadscale(ctx)

	t.startTailscaled(ctx)

	return
}

func pickRandomPorts(addr string, n int) []string {
	addrs := make([]string, n)
	for ix := range n {
		lis, err := net.Listen("tcp", addr+":0")
		Expect(err).ToNot(HaveOccurred())
		addrs[ix] = lis.Addr().String()
		defer lis.Close()
	}
	return addrs
}

func (t *testCtx) mkNamespace(ctx context.Context) {
	By("creating namespace")
	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "kube-headscale-"}}
	Expect(client.Create(ctx, &ns)).ToNot(HaveOccurred())
	DeferCleanup(func(ctx context.Context) {
		By("deleting namespace")
		client.Delete(ctx, &ns)
	})
	t.ns = ns.Name
}

func (t *testCtx) startHeadscale(ctx context.Context) {
	By("starting headscale")

	cfg := jobj{
		"log": jobj{
			"level": "DEBUG",
		},
		"server_url":          "http://" + t.hsServerAddr,
		"listen_addr":         t.hsServerAddr,
		"grpc_listen_addr":    t.hsGrpcAddr,
		"grpc_allow_insecure": true,
		"dns": jobj{
			"magic_dns":          false,
			"override_local_dns": false,
			"paths":              jarr{t.dnsPath},
		},
		"noise": jobj{
			"private_key_path": filepath.Join(t.tmp, "noise.key"),
		},
		"prefixes": jobj{
			"v4": "100.64.0.0/10",
		},
		"unix_socket": t.hsSocketPath,
		"database": jobj{
			"type": "sqlite",
			"sqlite": jobj{
				"path": filepath.Join("db.sqlite"),
			},
		},
		"policy": jobj{
			"mode": "database",
		},
		"derp": jobj{
			"server": jobj{
				"enabled":                                true,
				"stun_listen_addr":                       t.hsDerpAddr,
				"private_key_path":                       filepath.Join(t.tmp, "derp.key"),
				"automatically_add_embedded_derp_region": true,
				"region_id":                              999,
				"region_code":                            "headscale",
				"region_name":                            "Headscale Embedded DERP",
				"verify_clients":                         true,
			},
			"urls":                jarr{"http://" + t.sidecarAddr + "/derpmap"},
			"auto_update_enabled": true,
			"update_frequency":    "1s",
		},
	}

	cfgJSON, err := json.Marshal(&cfg)
	Expect(err).ToNot(HaveOccurred())
	cfgPath := filepath.Join(t.tmp, "config.yaml")
	Expect(os.WriteFile(cfgPath, cfgJSON, 0o600)).To(Succeed())

	t.startCommand("stopping headscale", map[string]string{"TS_DEBUG_KEY_PATH": debugKeyPath}, "bin/headscale", "serve", "-c", cfgPath)

	By("checking headscale health")
	Eventually(func() error {
		return t.runCommand(ctx, nil, "bin/headscale", "health", "-c", cfgPath)
	}).Should(Succeed())

	hsconn, err := grpc.NewClient("unix://"+t.hsSocketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		//grpc.WithContextDialer(func(ctx context.Context, s string) (net.Conn, error) {
		//	return (&net.Dialer{}).DialContext(ctx, "unix", headscaleSocket)
		//}),
	)
	Expect(err).ToNot(HaveOccurred())
	t.hs = hsv1.NewHeadscaleServiceClient(hsconn)
}

func (t *testCtx) startTailscaled(ctx context.Context) {
	By("starting tailscaled")
	t.tsSocketPath = filepath.Join(t.tmp, "tailscale.sock")
	t.startCommand("stopping tailscaled", nil, "bin/tailscaled", "-socket", t.tsSocketPath, "-tun", "userspace-networking")

	By("checking tailscaled health")
	Eventually(func() error {
		return t.runCommand(ctx, nil, "bin/tailscale", "--socket", t.tsSocketPath, "version", "--daemon")
	}).Should(Succeed())
}

func (t *testCtx) tailscaleLoginPreAuth(ctx context.Context, key string) {
	By("logging into headscale from tailscale")
	t.runCommand(ctx, nil, "bin/tailscale", "--socket", t.tsSocketPath, "login", "--login-server", "http://"+t.hsServerAddr, "--auth-key", key)

	By("checking tailscale health")
	Eventually(func() error {
		return t.runCommand(ctx, nil, "bin/tailscale", "--socket", t.tsSocketPath, "status")
	})
}

func (t *testCtx) startSidecar(ctx context.Context) {
	By("starting sidecar")

	t.startCommand("stopping sidecar", nil, "bin/kube-headscale.cover",
		"--kubeconfig", kubeconfigPath,
		"--namespace", t.ns,
		"--headscale-sock", t.hsSocketPath,
		"--headscale-grpc", t.hsGrpcAddr,
		"--headscale-insecure",
		"--dns-extra-records-path", t.dnsPath,
		"--apikey-rotate-before", "500ms",
		"--listen-addr", t.sidecarAddr,
	)
	By("checking sidecar health")
	Eventually(func() error {
		// TODO: Context
		_, err := http.Get("http://" + t.sidecarAddr + "/derpmap")
		return err
	}).Should(Succeed())
}

func mkStrToStr(s ...string) map[string]string {
	m := make(map[string]string, len(s)/2)
	for ix := range len(s) / 2 {
		m[s[2*ix]] = s[2*ix+1]
	}
	return m
}

func (t *testCtx) mkUser(ctx context.Context, name string) uint64 {
	resp, err := t.hs.CreateUser(ctx, &hsv1.CreateUserRequest{
		Name: name,
	})
	Expect(err).ToNot(HaveOccurred())
	return resp.User.Id
}

func (t *testCtx) mkPreAuthKey(ctx context.Context, user uint64) string {
	resp, err := t.hs.CreatePreAuthKey(ctx, &hsv1.CreatePreAuthKeyRequest{User: user, Expiration: timestamppb.New(time.Now().Add(5 * time.Minute))})
	Expect(err).ToNot(HaveOccurred())
	return resp.PreAuthKey.Key
}

func (t *testCtx) mkPolicyCM(ctx context.Context, strToStr ...string) corev1.ConfigMap {
	cm := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "kube-headscale-",
			Namespace:    t.ns,
			Labels: map[string]string{
				"kube-headscale.meln5674.io/policy": "1",
			},
		},
		Data: mkStrToStr(strToStr...),
	}
	Expect(client.Create(ctx, &cm)).To(Succeed())
	return cm
}

func (t *testCtx) mkDERPMapCM(ctx context.Context, strToStr ...string) {
	Expect(client.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "kube-headscale-",
			Namespace:    t.ns,
			Labels: map[string]string{
				"kube-headscale.meln5674.io/derpmap": "1",
			},
		},
		Data: mkStrToStr(strToStr...),
	})).To(Succeed())
}

func (t *testCtx) mkPreAuthKeySecret(ctx context.Context, user string) string {
	s := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "kube-headscale-",
			Namespace:    t.ns,
			Labels: map[string]string{
				"kube-headscale.meln5674.io/preauthkey": user,
			},
		},
	}
	Expect(client.Create(ctx, &s)).To(Succeed())
	return s.Name
}

func (t *testCtx) mkApiKeySecret(ctx context.Context, annotationStrToStr ...string) string {
	s := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "kube-headscale-",
			Namespace:    t.ns,
			Annotations:  mkStrToStr(annotationStrToStr...),
			Labels: map[string]string{
				"kube-headscale.meln5674.io/apikey": "1",
			},
		},
	}
	Expect(client.Create(ctx, &s)).To(Succeed())
	return s.Name
}

func (t *testCtx) mkApiKeySecretExpiresAt(ctx context.Context, expiration time.Time) string {
	return t.mkApiKeySecret(ctx, "kube-headscale.meln5674.io/apikey-expiration", expiration.Format(time.RFC3339))
}

func (t *testCtx) mkApiKeySecretExpiresAfter(ctx context.Context, lifetime time.Duration) string {
	return t.mkApiKeySecret(ctx, "kube-headscale.meln5674.io/apikey-lifetime", lifetime.String())
}

func (t *testCtx) expectPolicy(ctx context.Context, match gomegatypes.GomegaMatcher) {
	Eventually(func() error {
		_, err := t.hs.GetPolicy(ctx, &hsv1.GetPolicyRequest{})
		return err
	}, "5s").Should(Succeed())
	Eventually(func() jobj {
		resp, err := t.hs.GetPolicy(ctx, &hsv1.GetPolicyRequest{})
		Expect(err).ToNot(HaveOccurred())
		return parseJObj(resp.Policy)
	}, "5s").Should(match)
}

func (t *testCtx) expectDerpMap(ctx context.Context, match gomegatypes.GomegaMatcher) {
	Eventually(func() jobj {
		c := t.mkCommand(ctx, nil, "bin/tailscale", "--socket", t.tsSocketPath, "debug", "derp-map")
		c.Stdout = nil
		derpMapJSON, err := c.Output()
		GinkgoWriter.Write(derpMapJSON)
		Expect(err).ToNot(HaveOccurred())
		var derpMap jobj
		Expect(json.Unmarshal(derpMapJSON, &derpMap)).To(Succeed())
		return derpMap
	}, "5s", "1s").Should(match)
}

func (t *testCtx) startCommand(banner string, env map[string]string, cmd string, args ...string) *exec.Cmd {
	By(fmt.Sprintf("start: %s %v", cmd, args))
	c := t.mkCommand(t.ctx, env, cmd, args...)
	Expect(c.Start()).To(Succeed())
	DeferCleanup(func() {
		By(banner)
		c.Process.Signal(os.Interrupt)
		// headscale.Process.Kill()
		c.Wait()
	})

	return c
}

func (t *testCtx) runCommand(ctx context.Context, env map[string]string, cmd string, args ...string) error {
	By(fmt.Sprintf("run: %s %v", cmd, args))
	return t.mkCommand(ctx, env, cmd, args...).Run()
}

func (t *testCtx) mkCommand(ctx context.Context, env map[string]string, cmd string, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, cmd, args...)
	c.Env = append(c.Env, os.Environ()...)
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}
	c.Stdout = GinkgoWriter
	c.Stderr = GinkgoWriter
	return c
}

func (t *testCtx) connectToHeadscaleGRPC(apikey string) hsv1.HeadscaleServiceClient {
	hsconn, err := grpc.NewClient(t.hsGrpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(tokenAuth{
			token: apikey,
		}),
	)
	Expect(err).ToNot(HaveOccurred())
	return hsv1.NewHeadscaleServiceClient(hsconn)
}
