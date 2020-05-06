module k8s.io/helm

require (
	cloud.google.com/go v0.38.0
	github.com/Azure/go-ansiterm v0.0.0-20170929234023-d6e3b3328b78
	github.com/Azure/go-autorest v13.0.1+incompatible
	github.com/BurntSushi/toml v0.3.1
	github.com/DATA-DOG/go-sqlmock v1.4.1
	github.com/MakeNowJust/heredoc v0.0.0-20170808103936-bb23615498cd
	github.com/Masterminds/goutils v1.1.0
	github.com/Masterminds/semver v1.4.2
	github.com/Masterminds/sprig v2.20.0+incompatible
	github.com/Masterminds/vcs v1.11.1
	github.com/Microsoft/hcsshim v0.8.6 // indirect
	github.com/PuerkitoBio/purell v1.1.1
	github.com/PuerkitoBio/urlesc v0.0.0-20170810143723-de5bf2ad4578
	github.com/asaskevich/govalidator v0.0.0-20190424111038-f61b66f89f4a
	github.com/beorn7/perks v0.0.0-20180321164747-3a771d992973
	github.com/chai2010/gettext-go v0.0.0-20160711120539-c6fed771bfd5
	github.com/containerd/containerd v1.2.3 // indirect
	github.com/containerd/continuity v0.0.0-20190827140505-75bee3e2ccb6 // indirect
	github.com/containerd/cri v1.11.1 // indirect
	github.com/containerd/fifo v0.0.0-20190816180239-bda0ff6ed73c // indirect
	github.com/cpuguy83/go-md2man v1.0.10
	github.com/cyphar/filepath-securejoin v0.2.2
	github.com/davecgh/go-spew v1.1.1
	github.com/dgrijalva/jwt-go v3.2.0+incompatible
	github.com/docker/distribution v2.7.1+incompatible
	github.com/docker/docker v0.7.3-0.20190327010347-be7ac8be2ae0
	github.com/docker/go-events v0.0.0-20190806004212-e31b211e4f1c // indirect
	github.com/docker/spdystream v0.0.0-20160310174837-449fdfce4d96
	github.com/emicklei/go-restful v2.9.5+incompatible
	github.com/evanphx/json-patch v4.2.0+incompatible
	github.com/exponent-io/jsonpath v0.0.0-20151013193312-d6023ce2651d
	github.com/fatih/camelcase v1.0.0
	github.com/flant/logboek v0.3.1
	github.com/ghodss/yaml v1.0.1-0.20190212211648-25d852aebe32
	github.com/go-openapi/jsonpointer v0.19.3
	github.com/go-openapi/jsonreference v0.19.2
	github.com/go-openapi/spec v0.19.2
	github.com/go-openapi/swag v0.19.5
	github.com/gobwas/glob v0.2.3
	github.com/gofrs/flock v0.7.1
	github.com/gogo/googleapis v1.3.0 // indirect
	github.com/gogo/protobuf v1.3.0
	github.com/golang/groupcache v0.0.0-20160516000752-02826c3e7903
	github.com/golang/protobuf v1.3.1
	github.com/google/btree v0.0.0-20180813153112-4030bb1f1f0c
	github.com/google/go-cmp v0.3.0
	github.com/google/gofuzz v1.0.0
	github.com/google/uuid v1.1.1
	github.com/googleapis/gnostic v0.0.0-20170729233727-0c5108395e2d
	github.com/gophercloud/gophercloud v0.1.0
	github.com/gosuri/uitable v0.0.0-20160404203958-36ee7e946282
	github.com/gregjones/httpcache v0.0.0-20180305231024-9cad4c3443a7
	github.com/grpc-ecosystem/go-grpc-prometheus v1.2.0
	github.com/hashicorp/golang-lru v0.5.1
	github.com/heketi/rest v0.0.0-20180404230133-aa6a65207413 // indirect
	github.com/heketi/utils v0.0.0-20170317161834-435bc5bdfa64 // indirect
	github.com/huandu/xstrings v1.2.0
	github.com/imdario/mergo v0.3.5
	github.com/inconshreveable/mousetrap v1.0.0
	github.com/jmoiron/sqlx v1.2.0
	github.com/json-iterator/go v1.1.7
	github.com/konsorten/go-windows-terminal-sequences v1.0.1
	github.com/lib/pq v1.0.0
	github.com/liggitt/tabwriter v0.0.0-20181228230101-89fcab3d43de
	github.com/mailru/easyjson v0.0.0-20190626092158-b2ccc519800e
	github.com/mattn/go-runewidth v0.0.1
	github.com/matttproud/golang_protobuf_extensions v1.0.1
	github.com/mitchellh/go-wordwrap v1.0.0
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd
	github.com/modern-go/reflect2 v1.0.1
	github.com/opencontainers/go-digest v1.0.0-rc1
	github.com/peterbourgon/diskv v2.0.1+incompatible
	github.com/pkg/errors v0.8.1
	github.com/prometheus/client_golang v0.9.2
	github.com/prometheus/client_model v0.0.0-20180712105110-5c3871d89910
	github.com/prometheus/common v0.0.0-20181126121408-4724e9255275
	github.com/prometheus/procfs v0.0.0-20181204211112-1dc9a6cbc91a
	github.com/rubenv/sql-migrate v0.0.0-20190212093014-1007f53448d7
	github.com/russross/blackfriday v1.5.2
	github.com/shurcooL/sanitized_anchor_name v0.0.0-20151028001915-10ef21a441db
	github.com/sirupsen/logrus v1.4.2
	github.com/spf13/cobra v0.0.5
	github.com/spf13/pflag v1.0.5
	github.com/stretchr/testify v1.3.0
	github.com/technosophos/moniker v0.0.0-20180509230615-a5dbd03a2245
	golang.org/x/crypto v0.0.0-20190820162420-60c769a6c586
	golang.org/x/net v0.0.0-20191004110552-13f9640d40b9
	golang.org/x/oauth2 v0.0.0-20190604053449-0f29369cfe45
	golang.org/x/sync v0.0.0-20190911185100-cd5d95a43a6e
	golang.org/x/sys v0.0.0-20191026070338-33540a1f6037
	golang.org/x/text v0.3.2
	golang.org/x/time v0.0.0-20190308202827-9d24e82272b4
	google.golang.org/appengine v1.5.0
	google.golang.org/genproto v0.0.0-20190502173448-54afdca5d873
	google.golang.org/grpc v1.23.0
	gopkg.in/gorp.v1 v1.7.2
	gopkg.in/inf.v0 v0.9.1
	gopkg.in/square/go-jose.v2 v2.2.2
	gopkg.in/yaml.v2 v2.2.8
	gopkg.in/yaml.v3 v3.0.0-20200121175148-a6ecf24a6d71 // indirect
	k8s.io/api v0.16.7
	k8s.io/apiextensions-apiserver v0.0.0
	k8s.io/apimachinery v0.16.7
	k8s.io/apiserver v0.16.7
	k8s.io/cli-runtime v0.16.7
	k8s.io/client-go v0.16.7
	k8s.io/cloud-provider v0.16.7
	k8s.io/klog v1.0.0
	k8s.io/kube-openapi v0.0.0-20190816220812-743ec37842bf
	k8s.io/kubectl v0.0.0
	k8s.io/kubernetes v1.16.7
	k8s.io/utils v0.0.0-20190801114015-581e00157fb1
	sigs.k8s.io/kustomize v2.0.3+incompatible
	sigs.k8s.io/yaml v1.1.0
	vbom.ml/util v0.0.0-20160121211510-db5cfe13f5cc
)

replace k8s.io/api => k8s.io/api v0.16.7

replace k8s.io/apiextensions-apiserver => k8s.io/apiextensions-apiserver v0.16.7

replace k8s.io/apimachinery => k8s.io/apimachinery v0.16.8-beta.0

replace k8s.io/apiserver => k8s.io/apiserver v0.16.7

replace k8s.io/cli-runtime => k8s.io/cli-runtime v0.16.7

replace k8s.io/client-go => k8s.io/client-go v0.16.7

replace k8s.io/cloud-provider => k8s.io/cloud-provider v0.16.7

replace k8s.io/cluster-bootstrap => k8s.io/cluster-bootstrap v0.16.7

replace k8s.io/code-generator => k8s.io/code-generator v0.16.8-beta.0

replace k8s.io/component-base => k8s.io/component-base v0.16.7

replace k8s.io/cri-api => k8s.io/cri-api v0.16.8-beta.0

replace k8s.io/csi-translation-lib => k8s.io/csi-translation-lib v0.16.7

replace k8s.io/kube-aggregator => k8s.io/kube-aggregator v0.16.7

replace k8s.io/kube-controller-manager => k8s.io/kube-controller-manager v0.16.7

replace k8s.io/kube-proxy => k8s.io/kube-proxy v0.16.7

replace k8s.io/kube-scheduler => k8s.io/kube-scheduler v0.16.7

replace k8s.io/kubectl => k8s.io/kubectl v0.16.7

replace k8s.io/kubelet => k8s.io/kubelet v0.16.7

replace k8s.io/legacy-cloud-providers => k8s.io/legacy-cloud-providers v0.16.7

replace k8s.io/metrics => k8s.io/metrics v0.16.7

replace k8s.io/node-api => k8s.io/node-api v0.16.7

replace k8s.io/sample-apiserver => k8s.io/sample-apiserver v0.16.7

replace k8s.io/sample-cli-plugin => k8s.io/sample-cli-plugin v0.16.7

replace k8s.io/sample-controller => k8s.io/sample-controller v0.16.7

replace golang.org/x/sys => golang.org/x/sys v0.0.0-20190813064441-fde4db37ae7a

go 1.13
