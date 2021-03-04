package tencent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cnrancher/autok3s/pkg/cluster"
	"github.com/cnrancher/autok3s/pkg/common"
	"github.com/cnrancher/autok3s/pkg/hosts"
	"github.com/cnrancher/autok3s/pkg/providers"
	putil "github.com/cnrancher/autok3s/pkg/providers/utils"
	"github.com/cnrancher/autok3s/pkg/types"
	"github.com/cnrancher/autok3s/pkg/types/tencent"
	"github.com/cnrancher/autok3s/pkg/utils"
	"github.com/cnrancher/autok3s/pkg/viper"

	"github.com/rancher/wrangler/pkg/schemas"
	"github.com/sirupsen/logrus"
	tencentCommon "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	cvm "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/cvm/v20170312"
	tag "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/tag/v20180813"
	tke "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/tke/v20180525"
	vpc "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/vpc/v20170312"
	"golang.org/x/sync/syncmap"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	k3sVersion               = ""
	k3sChannel               = "stable"
	k3sInstallScript         = "http://rancher-mirror.cnrancher.com/k3s/k3s-install.sh"
	secretID                 = "secret-id"
	secretKey                = "secret-key"
	imageID                  = "img-pi0ii46r" /* Ubuntu Server 18.04.1 LTS x64 */
	instanceType             = "SA1.MEDIUM4"  /* CPU:2 Memory:4 */
	instanceChargeType       = "POSTPAID_BY_HOUR"
	internetMaxBandwidthOut  = "5"
	internetChargeType       = "TRAFFIC_POSTPAID_BY_HOUR"
	diskCategory             = "CLOUD_SSD"
	diskSize                 = "50"
	master                   = "0"
	worker                   = "0"
	ui                       = false
	defaultCidr              = "10.42.0.0/16"
	defaultRegion            = "ap-guangzhou"
	defaultZone              = "ap-guangzhou-3"
	defaultSecurityGroupName = "autok3s"
	vpcName                  = "autok3s-tencent-vpc"
	subnetName               = "autok3s-tencent-subnet"
	vpcCidrBlock             = "192.168.0.0/16"
	subnetCidrBlock          = "192.168.3.0/24"
	ipRange                  = "0.0.0.0/0"
	defaultUser              = "ubuntu"
)

// ProviderName is the name of this provider.
const ProviderName = "tencent"

var (
	k3sMirror        = "INSTALL_K3S_MIRROR=cn"
	dockerMirror     = ""
	deployCCMCommand = "echo \"%s\" | base64 -d | sudo tee \"%s/cloud-controller-manager.yaml\""
)

type checkFun func() error

type Tencent struct {
	types.Metadata  `json:",inline"`
	tencent.Options `json:",inline"`
	types.Status    `json:"status"`

	c      *cvm.Client
	v      *vpc.Client
	t      *tag.Client
	r      *tke.Client
	m      *sync.Map
	logger *logrus.Logger
}

func init() {
	providers.RegisterProvider(ProviderName, func() (providers.Provider, error) {
		return NewProvider(), nil
	})
}

func NewProvider() *Tencent {
	return &Tencent{
		Metadata: types.Metadata{
			Provider:      ProviderName,
			Master:        master,
			Worker:        worker,
			UI:            ui,
			K3sVersion:    k3sVersion,
			K3sChannel:    k3sChannel,
			InstallScript: k3sInstallScript,
			Cluster:       false,
		},
		Options: tencent.Options{
			ImageID:                 imageID,
			InstanceType:            instanceType,
			SystemDiskSize:          diskSize,
			SystemDiskType:          diskCategory,
			InternetMaxBandwidthOut: internetMaxBandwidthOut,
			PublicIPAssignedEIP:     false,
			Region:                  defaultRegion,
			Zone:                    defaultZone,
		},
		Status: types.Status{
			MasterNodes: make([]types.Node, 0),
			WorkerNodes: make([]types.Node, 0),
		},
		m: new(syncmap.Map),
	}
}

func (p *Tencent) GetProviderName() string {
	return ProviderName
}

func (p *Tencent) GenerateClusterName() {
	p.Name = fmt.Sprintf("%s.%s.%s", p.Name, p.Region, p.GetProviderName())
}

func (p *Tencent) CreateK3sCluster(ssh *types.SSH) (err error) {
	logFile, err := common.GetLogFile(p.Name)
	if err != nil {
		return err
	}
	c := &types.Cluster{
		Metadata: p.Metadata,
		Options:  p.Options,
		Status:   p.Status,
	}
	defer func() {
		if err != nil {
			p.logger.Errorf("[%s] failed to create cluster: %v", p.GetProviderName(), err)
			if c == nil {
				c = &types.Cluster{
					Metadata: p.Metadata,
					Options:  p.Options,
					Status:   p.Status,
				}
			}
			c.Status.Status = common.StatusFailed
			cluster.SaveClusterState(c, common.StatusFailed)
			os.Remove(filepath.Join(common.GetClusterStatePath(), fmt.Sprintf("%s_%s", p.Name, common.StatusCreating)))
		}
		if err == nil && len(p.Status.MasterNodes) > 0 {
			p.logger.Info(common.UsageInfoTitle)
			p.logger.Infof(common.UsageContext, p.Name)
			p.logger.Info(common.UsagePods)
			if p.UI {
				if p.CloudControllerManager {
					p.logger.Infof("K3s UI URL: https://<using `kubectl get svc -A` get UI address>:8999")
				} else {
					p.logger.Infof("K3s UI URL: https://%s:8999", p.Status.MasterNodes[0].PublicIPAddress[0])
				}
			}
			cluster.SaveClusterState(c, common.StatusRunning)
			// remove creating state file and save running state
			os.Remove(filepath.Join(common.GetClusterStatePath(), fmt.Sprintf("%s_%s", p.Name, common.StatusCreating)))
		}
		logFile.Close()
	}()
	os.Remove(filepath.Join(common.GetClusterStatePath(), fmt.Sprintf("%s_%s", p.Name, common.StatusFailed)))

	p.logger = common.NewLogger(common.Debug, logFile)
	p.logger.Infof("[%s] executing create logic...", p.GetProviderName())
	if ssh.User == "" {
		ssh.User = defaultUser
	}
	c.Status.Status = common.StatusCreating
	err = cluster.SaveClusterState(c, common.StatusCreating)
	if err != nil {
		return err
	}

	c, err = p.generateInstance(func() error {
		return nil
	}, ssh)
	if err != nil {
		return
	}

	c.Logger = p.logger
	// initialize K3s cluster.
	if err = cluster.InitK3sCluster(c); err != nil {
		return
	}
	p.logger.Infof("[%s] successfully executed create logic", p.GetProviderName())

	if option, ok := c.Options.(tencent.Options); ok {
		extraManifests := make([]string, 0)
		if c.CloudControllerManager {
			// deploy additional Tencent cloud-controller-manager manifests.
			tencentCCM := &tencent.CloudControllerManager{
				Region:                base64.StdEncoding.EncodeToString([]byte(option.Region)),
				VpcID:                 base64.StdEncoding.EncodeToString([]byte(option.VpcID)),
				NetworkRouteTableName: base64.StdEncoding.EncodeToString([]byte(option.NetworkRouteTableName)),
			}
			tencentCCM.SecretID = base64.StdEncoding.EncodeToString([]byte(option.SecretID))
			tencentCCM.SecretKey = base64.StdEncoding.EncodeToString([]byte(option.SecretKey))
			if p.ClusterCIDR == "" {
				p.ClusterCIDR = defaultCidr
			}
			tmpl := fmt.Sprintf(tencentCCMTmpl, tencentCCM.Region, tencentCCM.SecretID, tencentCCM.SecretKey,
				tencentCCM.VpcID, tencentCCM.NetworkRouteTableName, p.ClusterCIDR)

			extraManifests = append(extraManifests, fmt.Sprintf(deployCCMCommand,
				base64.StdEncoding.EncodeToString([]byte(tmpl)), common.K3sManifestsDir))
		}
		p.logger.Infof("[%s] start deploy tencent additional manifests", p.GetProviderName())
		if err := cluster.DeployExtraManifest(c, extraManifests); err != nil {
			return err
		}
		p.logger.Infof("[%s] successfully deploy tencent additional manifests", p.GetProviderName())
	}

	return nil
}

func (p *Tencent) JoinK3sNode(ssh *types.SSH) (err error) {
	if p.m == nil {
		p.m = new(syncmap.Map)
	}
	logFile, err := common.GetLogFile(p.Name)
	if err != nil {
		return err
	}
	c := &types.Cluster{
		Metadata: p.Metadata,
		Options:  p.Options,
		Status:   p.Status,
	}
	defer func() {
		if err == nil {
			cluster.SaveClusterState(c, common.StatusRunning)
		}
		// remove join state file and save running state
		os.Remove(filepath.Join(common.GetClusterStatePath(), fmt.Sprintf("%s_%s", p.Name, common.StatusJoin)))
		logFile.Close()
	}()

	p.logger = common.NewLogger(common.Debug, logFile)
	p.logger.Infof("[%s] executing join logic...", p.GetProviderName())
	if ssh.User == "" {
		ssh.User = defaultUser
	}

	c.Status.Status = "upgrading"
	err = cluster.SaveClusterState(c, common.StatusJoin)
	if err != nil {
		return err
	}
	c, err = p.generateInstance(p.joinCheck, ssh)
	if err != nil {
		return err
	}

	added := &types.Cluster{
		Metadata: c.Metadata,
		Options:  c.Options,
		Status:   types.Status{},
	}

	p.m.Range(func(key, value interface{}) bool {
		v := value.(types.Node)
		// filter the number of nodes that are not generated by current command.
		if v.Current {
			if v.Master {
				added.Status.MasterNodes = append(added.Status.MasterNodes, v)
			} else {
				added.Status.WorkerNodes = append(added.Status.WorkerNodes, v)
			}
		}
		return true
	})

	c.Logger = p.logger
	added.Logger = p.logger
	// join K3s node.
	if err := cluster.JoinK3sNode(c, added); err != nil {
		return err
	}

	p.logger.Infof("[%s] successfully executed join logic", p.GetProviderName())
	return nil
}

func (p *Tencent) Rollback() error {
	logFile, err := common.GetLogFile(p.Name)
	if err != nil {
		return err
	}
	p.logger = common.NewLogger(common.Debug, logFile)
	p.logger.Infof("[%s] executing rollback logic...", p.GetProviderName())

	ids := make([]string, 0)
	p.m.Range(func(key, value interface{}) bool {
		v := value.(types.Node)
		if v.RollBack {
			ids = append(ids, key.(string))
		}
		return true
	})

	p.logger.Infof("[%s] instances %s will be rollback", p.GetProviderName(), ids)

	if len(ids) > 0 {
		if p.PublicIPAssignedEIP {
			eips, err := p.describeAddresses(nil, tencentCommon.StringPtrs(ids))
			if err != nil {
				p.logger.Errorf("[%s] error when query eip info", p.GetProviderName())
			}
			var (
				eipIds  []string
				taskIds []uint64
			)
			for _, eip := range eips {
				eipIds = append(eipIds, *eip.AddressId)
				if taskID, err := p.disassociateAddress(*eip.AddressId); err != nil {
					p.logger.Warnf("[%s] disassociate eip [%s] error", p.GetProviderName(), *eip.AddressId)
				} else {
					taskIds = append(taskIds, taskID)
				}
			}
			for _, taskID := range taskIds {
				if err := p.describeVpcTaskResult(taskID); err != nil {
					p.logger.Warnf("[%s] disassociate eip task [%d] error", p.GetProviderName(), taskID)
				}
			}
			taskID, err := p.releaseAddresses(eipIds)
			if err != nil {
				p.logger.Warnf("[%s] release eip [%s] error", p.GetProviderName(), eipIds)
			}
			if err := p.describeVpcTaskResult(taskID); err != nil {
				p.logger.Warnf("[%s] release eip task [%d] error", p.GetProviderName(), taskID)
			}
		}

		wait.ErrWaitTimeout = fmt.Errorf("[%s] calling rollback error, please remove the cloud provider instances manually. region: %s, "+
			"instanceName: %s, msg: the maximum number of attempts reached", p.GetProviderName(), p.Region, ids)

		// retry 5 times, total 120 seconds.
		backoff := wait.Backoff{
			Duration: 30 * time.Second,
			Factor:   1,
			Steps:    5,
		}

		if err := wait.ExponentialBackoff(backoff, func() (bool, error) {
			if err := p.terminateInstances(ids); err != nil {
				return false, nil
			}
			return true, nil
		}); err != nil {
			return err
		}
	}

	// remove default key-pair folder
	err = os.RemoveAll(common.GetClusterPath(p.Name, p.GetProviderName()))
	if err != nil {
		return fmt.Errorf("[%s] remove cluster store folder (%s) error, msg: %v", p.GetProviderName(), common.GetClusterPath(p.Name, p.GetProviderName()), err)
	}

	p.logger.Infof("[%s] successfully executed rollback logic", p.GetProviderName())

	return logFile.Close()
}

func (p *Tencent) DeleteK3sCluster(f bool) error {
	isConfirmed := true

	if !f {
		isConfirmed = utils.AskForConfirmation(fmt.Sprintf("[%s] are you sure to delete cluster %s", p.GetProviderName(), p.Name))
	}

	if isConfirmed {
		logFile, err := common.GetLogFile(p.Name)
		if err != nil {
			return err
		}
		defer func() {
			logFile.Close()
			// remove log file
			os.Remove(filepath.Join(common.GetLogPath(), p.Name))
			// remove state file
			os.Remove(filepath.Join(common.GetClusterStatePath(), fmt.Sprintf("%s_%s", p.Name, common.StatusRunning)))
			os.Remove(filepath.Join(common.GetClusterStatePath(), fmt.Sprintf("%s_%s", p.Name, common.StatusFailed)))
		}()
		p.logger = common.NewLogger(common.Debug, logFile)
		p.logger.Infof("[%s] executing delete cluster logic...", p.GetProviderName())

		if err := p.generateClientSDK(); err != nil {
			return err
		}

		if err := p.deleteCluster(f); err != nil {
			return err
		}

		p.logger.Infof("[%s] successfully excuted delete cluster logic", p.GetProviderName())
	}

	return nil
}

func (p *Tencent) SSHK3sNode(ssh *types.SSH, ip string) error {
	p.logger = common.NewLogger(common.Debug, nil)
	p.logger.Infof("[%s] executing ssh logic...", p.GetProviderName())

	if err := p.generateClientSDK(); err != nil {
		return err
	}

	instanceList, err := p.syncClusterInstance(ssh)
	if err != nil {
		return err
	}

	ids := make(map[string]string, len(instanceList))
	for _, instance := range instanceList {
		if instance.PublicIpAddresses != nil {
			instanceInfo := *instance.PublicIpAddresses[0]
			for _, t := range instance.Tags {
				if *t.Key != "master" && *t.Key != "worker" {
					continue
				}
				if *t.Value == "true" {
					instanceInfo = fmt.Sprintf("%s (%s)", instanceInfo, *t.Key)
					break
				}
			}
			if *instance.InstanceState != tencent.Running {
				instanceInfo = fmt.Sprintf("%s - Unhealthy(instance is %s)", instanceInfo, *instance.InstanceState)
			}
			ids[*instance.InstanceId] = instanceInfo
		}
	}
	// sync master/worker count
	p.Metadata.Master = strconv.Itoa(len(p.Status.MasterNodes))
	p.Metadata.Worker = strconv.Itoa(len(p.Status.WorkerNodes))
	c := &types.Cluster{
		Metadata: p.Metadata,
		Options:  p.Options,
		Status:   p.Status,
	}
	err = cluster.SaveState(c)

	if err != nil {
		return fmt.Errorf("[%s] synchronizing .state file error, msg: [%v]", p.GetProviderName(), err)
	}

	if ip == "" {
		ip = strings.Split(utils.AskForSelectItem(fmt.Sprintf("[%s] choose ssh node to connect", p.GetProviderName()), ids), " (")[0]
	}

	if ip == "" {
		return fmt.Errorf("[%s] choose incorrect ssh node", p.GetProviderName())
	}

	// ssh K3s node.
	if err := cluster.SSHK3sNode(ip, c, ssh); err != nil {
		return err
	}

	p.logger.Infof("[%s] successfully executed ssh logic", p.GetProviderName())

	return nil
}

func (p *Tencent) IsClusterExist() (bool, []string, error) {
	ids := make([]string, 0)

	if p.c == nil {
		if err := p.generateClientSDK(); err != nil {
			return false, ids, err
		}
	}
	instanceList, err := p.describeInstances()
	if err != nil {
		return false, ids, err
	}
	if len(instanceList) > 0 {
		for _, resource := range instanceList {
			ids = append(ids, *resource.InstanceId)
		}
		return true, ids, nil
	}
	return false, ids, nil
}

func (p *Tencent) GenerateMasterExtraArgs(cluster *types.Cluster, master types.Node) string {
	if option, ok := cluster.Options.(tencent.Options); ok {
		if cluster.CloudControllerManager {
			extraArgs := fmt.Sprintf(" --kubelet-arg=cloud-provider=external --kubelet-arg=node-status-update-frequency=30s --kubelet-arg=provider-id=tencentcloud:///%s/%s --node-name=%s",
				option.Zone, master.InstanceID, master.InternalIPAddress[0])
			return extraArgs
		}
	}
	return ""
}

func (p *Tencent) GenerateWorkerExtraArgs(cluster *types.Cluster, worker types.Node) string {
	return p.GenerateMasterExtraArgs(cluster, worker)
}

func (p *Tencent) GetCluster(kubecfg string) *types.ClusterInfo {
	p.logger = common.NewLogger(common.Debug, nil)
	c := &types.ClusterInfo{
		Name:     p.Name,
		Region:   p.Region,
		Zone:     p.Zone,
		Provider: p.GetProviderName(),
	}
	client, err := cluster.GetClusterConfig(p.Name, kubecfg)
	if err != nil {
		p.logger.Errorf("[%s] failed to generate kube client for cluster %s: %v", p.GetProviderName(), p.Name, err)
		c.Status = types.ClusterStatusUnknown
		c.Version = types.ClusterStatusUnknown
		return c
	}
	c.Status = cluster.GetClusterStatus(client)
	if c.Status == types.ClusterStatusRunning {
		c.Version = cluster.GetClusterVersion(client)
	} else {
		c.Version = types.ClusterStatusUnknown
	}

	if p.c == nil {
		if err := p.generateClientSDK(); err != nil {
			p.logger.Errorf("[%s] failed to generate tencent client sdk for cluster %s: %v", p.GetProviderName(), p.Name, err)
			c.Master = "0"
			c.Worker = "0"
			return c
		}
	}
	instanceList, err := p.describeInstances()
	if err != nil {
		p.logger.Errorf("[%s] failed to get instance for cluster %s: %v", p.GetProviderName(), p.Name, err)
		c.Master = "0"
		c.Worker = "0"
		return c
	}

	masterCount := 0
	workerCount := 0
	for _, ins := range instanceList {
		isMaster := false
		for _, t := range ins.Tags {
			if *t.Key == "master" && *t.Value == "true" {
				isMaster = true
				masterCount++
				break
			}
		}
		if !isMaster {
			workerCount++
		}
	}
	c.Master = strconv.Itoa(masterCount)
	c.Worker = strconv.Itoa(workerCount)

	return c
}

func (p *Tencent) DescribeCluster(kubecfg string) *types.ClusterInfo {
	p.logger = common.NewLogger(common.Debug, nil)
	c := &types.ClusterInfo{
		Name:     p.Name,
		Region:   p.Region,
		Zone:     p.Zone,
		Provider: p.GetProviderName(),
	}
	client, err := cluster.GetClusterConfig(p.Name, kubecfg)
	if err != nil {
		p.logger.Errorf("[%s] failed to generate kube client for cluster %s: %v", p.GetProviderName(), p.Name, err)
		c.Status = types.ClusterStatusUnknown
		c.Version = types.ClusterStatusUnknown
		return c
	}
	c.Status = cluster.GetClusterStatus(client)
	if p.c == nil {
		if err := p.generateClientSDK(); err != nil {
			p.logger.Errorf("[%s] failed to generate tencent client sdk for cluster %s: %v", p.GetProviderName(), p.Name, err)
			c.Master = "0"
			c.Worker = "0"
			return c
		}
	}
	instanceList, err := p.describeInstances()
	if err != nil {
		p.logger.Errorf("[%s] failed to get instance for cluster %s: %v", p.GetProviderName(), p.Name, err)
		c.Master = "0"
		c.Worker = "0"
		return c
	}
	instanceNodes := make([]types.ClusterNode, 0)
	masterCount := 0
	workerCount := 0
	for _, instance := range instanceList {
		n := types.ClusterNode{
			InstanceID:              *instance.InstanceId,
			InstanceStatus:          *instance.InstanceState,
			InternalIP:              tencentCommon.StringValues(instance.PrivateIpAddresses),
			ExternalIP:              tencentCommon.StringValues(instance.PublicIpAddresses),
			Status:                  types.ClusterStatusUnknown,
			ContainerRuntimeVersion: types.ClusterStatusUnknown,
			Version:                 types.ClusterStatusUnknown,
		}
		isMaster := false
		for _, t := range instance.Tags {
			if *t.Key == "master" && *t.Value == "true" {
				isMaster = true
				masterCount++
				break
			}
		}
		if !isMaster {
			workerCount++
		}
		instanceNodes = append(instanceNodes, n)
	}
	c.Master = strconv.Itoa(masterCount)
	c.Worker = strconv.Itoa(workerCount)
	c.Nodes = instanceNodes

	if c.Status == types.ClusterStatusRunning {
		c.Version = cluster.GetClusterVersion(client)
		nodes, err := cluster.DescribeClusterNodes(client, instanceNodes)
		if err != nil {
			p.logger.Errorf("[%s] failed to list nodes of cluster %s: %v", p.GetProviderName(), p.Name, err)
			return c
		}
		c.Nodes = nodes
	} else {
		c.Version = types.ClusterStatusUnknown
	}

	return c
}

func (p *Tencent) GetClusterConfig() (map[string]schemas.Field, error) {
	config := p.GetSSHConfig()
	sshConfig, err := utils.ConvertToFields(*config)
	if err != nil {
		return nil, err
	}
	metaConfig, err := utils.ConvertToFields(p.Metadata)
	if err != nil {
		return nil, err
	}
	for k, v := range sshConfig {
		metaConfig[k] = v
	}
	return metaConfig, nil
}

func (p *Tencent) GetProviderOption() (map[string]schemas.Field, error) {
	return utils.ConvertToFields(p.Options)
}

func (p *Tencent) SetConfig(config []byte) error {
	c := types.Cluster{}
	err := json.Unmarshal(config, &c)
	if err != nil {
		return err
	}
	sourceMeta := reflect.ValueOf(&p.Metadata).Elem()
	targetMeta := reflect.ValueOf(&c.Metadata).Elem()
	utils.MergeConfig(sourceMeta, targetMeta)
	sourceOption := reflect.ValueOf(&p.Options).Elem()
	b, err := json.Marshal(c.Options)
	if err != nil {
		return err
	}
	opt := &tencent.Options{}
	err = json.Unmarshal(b, opt)
	if err != nil {
		return err
	}
	targetOption := reflect.ValueOf(opt).Elem()
	utils.MergeConfig(sourceOption, targetOption)

	return nil
}

func (p *Tencent) generateClientSDK() error {
	if p.SecretID == "" {
		p.SecretID = viper.GetString(p.GetProviderName(), secretID)
	}

	if p.SecretKey == "" {
		p.SecretKey = viper.GetString(p.GetProviderName(), secretKey)
	}

	credential := tencentCommon.NewCredential(
		p.SecretID,
		p.SecretKey,
	)
	cpf := profile.NewClientProfile()
	if p.EndpointURL != "" {
		cpf.HttpProfile.Endpoint = p.EndpointURL
	}
	if client, err := cvm.NewClient(credential, p.Region, cpf); err == nil {
		p.c = client
	} else {
		return err
	}

	if vpcClient, err := vpc.NewClient(credential, p.Region, cpf); err == nil {
		p.v = vpcClient
	} else {
		return err
	}

	// region for tag clients is not necessary
	if tagClient, err := tag.NewClient(credential, p.Region, cpf); err == nil {
		p.t = tagClient
	} else {
		return err
	}

	if tkeClient, err := tke.NewClient(credential, p.Region, cpf); err == nil {
		p.r = tkeClient
	} else {
		return err
	}
	return nil
}

func (p *Tencent) generateInstance(fn checkFun, ssh *types.SSH) (*types.Cluster, error) {
	var err error

	if err = p.generateClientSDK(); err != nil {
		return nil, err
	}

	if err = fn(); err != nil {
		return nil, err
	}

	// create key pair
	pk, err := putil.CreateKeyPair(ssh, p.GetProviderName(), p.Name, p.KeyIds)
	if err != nil {
		return nil, fmt.Errorf("[%s] Failed to create key pair: %v", p.GetProviderName(), err)
	}

	masterNum, _ := strconv.Atoi(p.Master)
	workerNum, _ := strconv.Atoi(p.Worker)

	p.logger.Infof("[%s] %d masters and %d workers will be added", p.GetProviderName(), masterNum, workerNum)

	if p.VpcID == "" {
		// config default vpc and subnet
		err = p.configNetwork()
		if err != nil {
			return nil, err
		}
	}

	if p.SecurityGroupIds == "" {
		// config default security groups
		err = p.configSecurityGroup()
		if err != nil {
			return nil, err
		}
	}

	needUploadKeyPair := false
	if ssh.Password == "" && p.KeyIds == "" {
		needUploadKeyPair = true
		ssh.Password = putil.RandomPassword()
		p.logger.Infof("[%s] launching instance with auto-generated password...", p.GetProviderName())
	}

	// run ecs master instances.
	if masterNum > 0 {
		p.logger.Infof("[%s] %d number of master instances will be created", p.GetProviderName(), masterNum)
		if err := p.runInstances(masterNum, true, ssh.Password); err != nil {
			return nil, err
		}
		p.logger.Infof("[%s] %d number of master instances successfully created", p.GetProviderName(), masterNum)
	}

	// run ecs worker instances.
	if workerNum > 0 {
		p.logger.Infof("[%s] %d number of worker instances will be created", p.GetProviderName(), workerNum)
		if err := p.runInstances(workerNum, false, ssh.Password); err != nil {
			return nil, err
		}
		p.logger.Infof("[%s] %d number of worker instances successfully created", p.GetProviderName(), workerNum)
	}

	// wait ecs instances to be running status.
	if err = p.getInstanceStatus(tencent.StatusRunning); err != nil {
		return nil, err
	}

	var eipTaskIds []uint64

	// allocate eip for master
	if masterNum > 0 && p.PublicIPAssignedEIP {
		taskIDs, err := p.allocateEIPForInstance(masterNum, true)
		if err != nil {
			return nil, err
		}
		eipTaskIds = append(eipTaskIds, taskIDs...)
	}

	// allocate eip for worker
	if workerNum > 0 && p.PublicIPAssignedEIP {
		taskIDs, err := p.allocateEIPForInstance(workerNum, false)
		if err != nil {
			return nil, err
		}
		eipTaskIds = append(eipTaskIds, taskIDs...)
	}

	// wait eip to be InUse status.
	if p.PublicIPAssignedEIP {
		for _, taskID := range eipTaskIds {
			if err = p.describeVpcTaskResult(taskID); err != nil {
				return nil, err
			}
		}
	}

	// assemble instance status.
	var c *types.Cluster
	if c, err = p.assembleInstanceStatus(ssh, needUploadKeyPair, string(pk)); err != nil {
		return nil, err
	}

	c.Mirror = k3sMirror
	c.DockerMirror = dockerMirror

	if _, ok := c.Options.(tencent.Options); ok {
		if c.CloudControllerManager {
			c.MasterExtraArgs += " --disable-cloud-controller --no-deploy servicelb,traefik"
		}
	}

	return c, nil
}

func (p *Tencent) deleteCluster(f bool) error {
	exist, ids, err := p.IsClusterExist()

	if err != nil {
		return err
	}

	if !exist {
		if !f {
			return fmt.Errorf("[%s] calling preflight error: cluster name `%s` do not exist", p.GetProviderName(), p.Name)
		}
		return nil
	}

	taggedResource, err := p.describeResourcesByTags()
	if err != nil {
		p.logger.Errorf("[%s] error when query tagged eip(s), message: %v", p.GetProviderName(), err)
	}
	var eipIds []string
	if len(taggedResource) > 0 {
		var taskIds []uint64
		for _, resource := range taggedResource {
			if strings.EqualFold(tencent.ServiceTypeEIP, *resource.ServiceType) &&
				strings.EqualFold(tencent.ResourcePrefixEIP, *resource.ResourcePrefix) {
				eipID := *resource.ResourceId
				if eipID != "" {
					taskID, err := p.disassociateAddress(eipID)
					if err != nil {
						p.logger.Errorf("[%s] error when query task eip disassociate progress, message: %v", p.GetProviderName(), err)
					}
					eipIds = append(eipIds, eipID)
					if taskID != 0 {
						taskIds = append(taskIds, taskID)
					}
				}
			}
		}
		for _, taskID := range taskIds {
			if err := p.describeVpcTaskResult(taskID); err != nil {
				p.logger.Errorf("[%s] error when query eip disassociate task result, message: %v", p.GetProviderName(), err)
			}
		}
	} else {
		p.logger.Warnf("[%s] failed to query tagged eip", p.GetProviderName())
	}
	if len(eipIds) > 0 {
		taskID, err := p.releaseAddresses(eipIds)
		if err != nil {
			p.logger.Errorf("[%s] failed to release tagged eip, message: %v", p.GetProviderName(), err)
		}
		if err := p.describeVpcTaskResult(taskID); err != nil {
			p.logger.Errorf("[%s] failed to query release eip task result, message: %v", p.GetProviderName(), err)
		}
	}

	if err == nil && len(ids) > 0 {
		p.logger.Infof("[%s] cluster %s will be deleted", p.GetProviderName(), p.Name)

		err := p.terminateInstances(ids)
		if err != nil {
			return fmt.Errorf("[%s] calling deleteInstance error, msg: %v", p.GetProviderName(), err)
		}
	}

	if err != nil && !f {
		return fmt.Errorf("[%s] calling deleteCluster error, msg: %v", p.GetProviderName(), err)
	}

	err = cluster.OverwriteCfg(p.Name)

	if err != nil && !f {
		return fmt.Errorf("[%s] synchronizing .cfg file error, msg: %v", p.GetProviderName(), err)
	}

	err = cluster.DeleteState(p.Name, p.Provider)

	if err != nil && !f {
		return fmt.Errorf("[%s] synchronizing .state file error, msg: %v", p.GetProviderName(), err)
	}

	// remove default key-pair folder
	err = os.RemoveAll(common.GetClusterPath(p.Name, p.GetProviderName()))
	if err != nil && !f {
		return fmt.Errorf("[%s] remove cluster store folder (%s) error, msg: %v", p.GetProviderName(), common.GetClusterPath(p.Name, p.GetProviderName()), err)
	}
	// remove log file
	os.Remove(filepath.Join(common.GetLogPath(), p.Name))
	p.logger.Infof("[%s] successfully deleted cluster %s", p.GetProviderName(), p.Name)

	return nil
}

func (p *Tencent) CreateCheck(ssh *types.SSH) error {
	if p.KeyIds != "" && ssh.SSHKeyPath == "" {
		return fmt.Errorf("[%s] calling preflight error: --ssh-key-path must set with --key-pair %s", p.GetProviderName(), p.KeyIds)
	}
	masterNum, err := strconv.Atoi(p.Master)
	if masterNum < 1 || err != nil {
		return fmt.Errorf("[%s] calling preflight error: `--master` number must >= 1",
			p.GetProviderName())
	}
	if masterNum > 1 && !p.Cluster && p.DataStore == "" {
		return fmt.Errorf("[%s] calling preflight error: need to set `--cluster` or `--datastore` when `--master` number > 1",
			p.GetProviderName())
	}

	if strings.Contains(p.MasterExtraArgs, "--datastore-endpoint") && p.DataStore != "" {
		return fmt.Errorf("[%s] calling preflight error: `--masterExtraArgs='--datastore-endpoint'` is duplicated with `--datastore`",
			p.GetProviderName())
	}

	exist, _, err := p.IsClusterExist()
	if err != nil {
		return err
	}

	if exist {
		context := strings.Split(p.Name, ".")
		return fmt.Errorf("[%s] calling preflight error: cluster `%s` at region %s is already exist",
			p.GetProviderName(), context[0], p.Region)
	}

	if p.Region != defaultRegion && p.Zone == defaultZone && p.VpcID == "" {
		return fmt.Errorf("[%s] calling preflight error: must set `--zone` in specified region %s to create default vpc or set exist `--vpc xxx --subnet xxx` in specified region", p.GetProviderName(), p.Region)
	}

	if p.CloudControllerManager && p.NetworkRouteTableName == "" {
		return fmt.Errorf("[%s] calling preflight error: must set `--router` if enabled tencent cloud manager",
			p.GetProviderName())
	}

	return nil
}

func (p *Tencent) joinCheck() error {
	if p.Master == "0" && p.Worker == "0" {
		return fmt.Errorf("[%s] calling preflight error: `--master` or `--worker` number must >= 1", p.GetProviderName())
	}
	if strings.Contains(p.MasterExtraArgs, "--datastore-endpoint") && p.DataStore != "" {
		return fmt.Errorf("[%s] calling preflight error: `--masterExtraArgs='--datastore-endpoint'` is duplicated with `--datastore`",
			p.GetProviderName())
	}

	exist, ids, err := p.IsClusterExist()

	if err != nil {
		return err
	}

	if !exist {
		return fmt.Errorf("[%s] calling preflight error: cluster name `%s` do not exist",
			p.GetProviderName(), p.Name)
	}

	// remove invalid worker nodes from .state file.
	workers := make([]types.Node, 0)
	for _, w := range p.WorkerNodes {
		for _, e := range ids {
			if e == w.InstanceID {
				workers = append(workers, w)
				break
			}
		}
	}
	p.WorkerNodes = workers

	return nil
}

func (p *Tencent) assembleInstanceStatus(ssh *types.SSH, uploadKeyPair bool, publicKey string) (*types.Cluster, error) {
	instanceList, err := p.describeInstances()
	if err != nil {
		return nil, fmt.Errorf("[%s] failed to list instance for cluster %s, region: %s, zone: %s: %v",
			p.GetProviderName(), p.Name, p.Region, p.Zone, err)
	}

	for _, status := range instanceList {
		InstanceID := *status.InstanceId
		var eip []string
		if p.PublicIPAssignedEIP {
			eipInfos, err := p.describeAddresses(nil, []*string{status.InstanceId})
			if err != nil {
				p.logger.Errorf("[%s] error when query eip info of instance:[%s]", p.GetProviderName(), *status.InstanceId)
				return nil, err
			}
			for _, eipInfo := range eipInfos {
				eip = append(eip, *eipInfo.AddressId)
			}
		}
		if value, ok := p.m.Load(InstanceID); ok {
			v := value.(types.Node)
			// add only nodes that run the current command.
			v.Current = true
			v.InternalIPAddress = tencentCommon.StringValues(status.PrivateIpAddresses)
			v.PublicIPAddress = tencentCommon.StringValues(status.PublicIpAddresses)
			v.EipAllocationIds = eip
			v.SSH = *ssh
			// check upload keypair
			if uploadKeyPair {
				p.logger.Infof("[%s] waiting for upload keypair...", p.GetProviderName())
				if err := p.uploadKeyPair(v, publicKey); err != nil {
					return nil, err
				}
				v.SSH.Password = ""
			}
			p.m.Store(InstanceID, v)
			continue
		}

		master := false
		for _, tagPtr := range status.Tags {
			if strings.EqualFold(*tagPtr.Key, "master") && strings.EqualFold(*tagPtr.Value, "true") {
				master = true
				break
			}
		}
		p.m.Store(InstanceID, types.Node{
			Master:            master,
			RollBack:          false,
			InstanceID:        InstanceID,
			InstanceStatus:    tencent.StatusRunning,
			InternalIPAddress: tencentCommon.StringValues(status.PrivateIpAddresses),
			EipAllocationIds:  eip,
			PublicIPAddress:   tencentCommon.StringValues(status.PublicIpAddresses)})

	}
	p.syncNodeStatusWithInstance(ssh)

	return &types.Cluster{
		Metadata: p.Metadata,
		Options:  p.Options,
		Status:   p.Status,
	}, nil
}

func (p *Tencent) runInstances(num int, master bool, password string) error {
	request := cvm.NewRunInstancesRequest()

	diskSize, _ := strconv.ParseInt(p.SystemDiskSize, 10, 64)
	bandwidth, _ := strconv.ParseInt(p.InternetMaxBandwidthOut, 10, 64)

	request.InstanceCount = tencentCommon.Int64Ptr(int64(num))
	request.ImageId = tencentCommon.StringPtr(p.ImageID)
	request.InstanceType = tencentCommon.StringPtr(p.InstanceType)
	request.Placement = &cvm.Placement{
		Zone: tencentCommon.StringPtr(p.Zone),
	}
	request.InstanceChargeType = tencentCommon.StringPtr(instanceChargeType)
	request.SecurityGroupIds = tencentCommon.StringPtrs(strings.Split(p.SecurityGroupIds, ","))
	request.VirtualPrivateCloud = &cvm.VirtualPrivateCloud{
		SubnetId: tencentCommon.StringPtr(p.SubnetID),
		VpcId:    tencentCommon.StringPtr(p.VpcID),
	}
	request.SystemDisk = &cvm.SystemDisk{
		DiskType: tencentCommon.StringPtr(p.SystemDiskType),
		DiskSize: tencentCommon.Int64Ptr(diskSize),
	}
	loginSettings := &cvm.LoginSettings{}
	if password != "" {
		loginSettings.Password = tencentCommon.StringPtr(password)
	}
	if p.KeyIds != "" {
		// only support bind one though it's array
		loginSettings.KeyIds = tencentCommon.StringPtrs([]string{p.KeyIds})
	}
	request.LoginSettings = loginSettings
	request.InternetAccessible = &cvm.InternetAccessible{
		InternetChargeType:      tencentCommon.StringPtr(internetChargeType),
		InternetMaxBandwidthOut: tencentCommon.Int64Ptr(bandwidth),
		PublicIpAssigned:        tencentCommon.BoolPtr(!p.PublicIPAssignedEIP),
	}

	// set instance tags
	tags := []*cvm.Tag{
		{Key: tencentCommon.StringPtr("autok3s"), Value: tencentCommon.StringPtr("true")},
		{Key: tencentCommon.StringPtr("cluster"), Value: tencentCommon.StringPtr(common.TagClusterPrefix + p.Name)},
	}

	for k, v := range p.Tags {
		tags = append(tags, &cvm.Tag{Key: tencentCommon.StringPtr(k), Value: tencentCommon.StringPtr(v)})
	}

	if master {
		request.InstanceName = tencentCommon.StringPtr(fmt.Sprintf(common.MasterInstanceName, p.Name))
		tags = append(tags, &cvm.Tag{Key: tencentCommon.StringPtr("master"), Value: tencentCommon.StringPtr("true")})
	} else {
		request.InstanceName = tencentCommon.StringPtr(fmt.Sprintf(common.WorkerInstanceName, p.Name))
		tags = append(tags, &cvm.Tag{Key: tencentCommon.StringPtr("worker"), Value: tencentCommon.StringPtr("true")})
	}
	request.TagSpecification = []*cvm.TagSpecification{{ResourceType: tencentCommon.StringPtr("instance"), Tags: tags}}

	response, err := p.c.RunInstances(request)
	if err != nil || len(response.Response.InstanceIdSet) != num {
		return fmt.Errorf("[%s] calling runInstances error. region: %s, zone: %s, "+"instanceName: %s, msg: [%v]",
			p.GetProviderName(), p.Region, p.Zone, *request.InstanceName, err)
	}
	for _, id := range response.Response.InstanceIdSet {
		p.m.Store(*id, types.Node{Master: master, RollBack: true, InstanceID: *id, InstanceStatus: tencent.StatusPending})
	}

	return nil
}

func (p *Tencent) describeInstances() ([]*cvm.Instance, error) {
	request := cvm.NewDescribeInstancesRequest()

	limit := int64(20)
	request.Limit = tencentCommon.Int64Ptr(limit)
	// If there are multiple Filters, between the Filters is a logical AND (AND)
	// If there are multiple Values in the same Filter, between Values under the same Filter is a logical OR (OR)
	request.Filters = []*cvm.Filter{
		{Name: tencentCommon.StringPtr("tag:autok3s"), Values: tencentCommon.StringPtrs([]string{"true"})},
		{Name: tencentCommon.StringPtr("tag:cluster"), Values: tencentCommon.StringPtrs([]string{common.TagClusterPrefix + p.Name})},
	}
	offset := int64(0)
	index := int64(0)
	instanceList := make([]*cvm.Instance, 0)
	for {
		response, err := p.c.DescribeInstances(request)
		if err != nil {
			return nil, err
		}
		if response.Response == nil || response.Response.InstanceSet == nil || len(response.Response.InstanceSet) == 0 {
			break
		}
		total := *response.Response.TotalCount
		for _, ins := range response.Response.InstanceSet {
			instanceList = append(instanceList, ins)
		}
		offset = limit*index + limit
		index = index + 1
		if offset >= total {
			break
		}
		request.Offset = tencentCommon.Int64Ptr(offset)
	}

	return instanceList, nil
}

func (p *Tencent) terminateInstances(instanceIds []string) error {
	request := cvm.NewTerminateInstancesRequest()

	request.InstanceIds = tencentCommon.StringPtrs(instanceIds)

	_, err := p.c.TerminateInstances(request)

	if err != nil {
		return fmt.Errorf("[%s] calling deleteInstance error, msg: %v", p.GetProviderName(), err)
	}

	return nil
}

func (p *Tencent) allocateAddresses(num int) ([]*string, uint64, error) {
	request := vpc.NewAllocateAddressesRequest()

	request.AddressCount = tencentCommon.Int64Ptr(int64(num))
	request.InternetChargeType = tencentCommon.StringPtr(internetChargeType)
	internetMaxBandwidthOut, _ := strconv.ParseInt(p.InternetMaxBandwidthOut, 10, 64)
	request.InternetMaxBandwidthOut = tencentCommon.Int64Ptr(internetMaxBandwidthOut)
	request.Tags = []*vpc.Tag{
		{Key: tencentCommon.StringPtr("autok3s"), Value: tencentCommon.StringPtr("true")},
		{Key: tencentCommon.StringPtr("cluster"), Value: tencentCommon.StringPtr(common.TagClusterPrefix + p.Name)},
	}
	response, err := p.v.AllocateAddresses(request)
	if err != nil {
		return nil, 0, fmt.Errorf("[%s] calling allocateAddresses error, msg: %v", p.GetProviderName(), err)
	}
	taskID, err := strconv.ParseUint(*response.Response.TaskId, 10, 64)
	if err != nil {
		return nil, 0, fmt.Errorf("[%s] error when convert taskID: %s", p.GetProviderName(), *response.Response.TaskId)
	}
	return response.Response.AddressSet, taskID, nil
}

func (p *Tencent) releaseAddresses(addressIds []string) (uint64, error) {
	request := vpc.NewReleaseAddressesRequest()

	request.AddressIds = tencentCommon.StringPtrs(addressIds)

	response, err := p.v.ReleaseAddresses(request)

	if err != nil {
		return 0, fmt.Errorf("[%s] calling releaseAddresses error, msg: %v", p.GetProviderName(), err)
	}
	taskID, err := strconv.ParseUint(*response.Response.TaskId, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("[%s] error when convert taskID: %s", p.GetProviderName(), *response.Response.TaskId)
	}
	return taskID, nil
}

func (p *Tencent) describeAddresses(addressIds, instanceIds []*string) ([]*vpc.Address, error) {
	request := vpc.NewDescribeAddressesRequest()

	if len(instanceIds) <= 0 {
		request.AddressIds = addressIds
	} else {
		filters := []*vpc.Filter{
			{Name: tencentCommon.StringPtr("instance-id"), Values: instanceIds},
		}
		if len(addressIds) > 0 {
			filters = append(filters, &vpc.Filter{Name: tencentCommon.StringPtr("address-id"), Values: addressIds})
		}
		request.Filters = filters
	}

	response, err := p.v.DescribeAddresses(request)

	if err != nil {
		return nil, fmt.Errorf("[%s] calling describeAddresses error, msg: %v", p.GetProviderName(), err)
	}

	return response.Response.AddressSet, nil
}

func (p *Tencent) associateAddress(addressID, instanceID string) (uint64, error) {
	request := vpc.NewAssociateAddressRequest()

	request.AddressId = tencentCommon.StringPtr(addressID)
	request.InstanceId = tencentCommon.StringPtr(instanceID)

	response, err := p.v.AssociateAddress(request)

	if err != nil {
		return 0, fmt.Errorf("[%s] calling associateAddress error, msg: %v", p.GetProviderName(), err)
	}
	taskID, err := strconv.ParseUint(*response.Response.TaskId, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("[%s] error when convert taskID: %s", p.GetProviderName(), *response.Response.TaskId)
	}
	return taskID, nil
}

func (p *Tencent) disassociateAddress(addressID string) (uint64, error) {
	request := vpc.NewDisassociateAddressRequest()

	request.AddressId = tencentCommon.StringPtr(addressID)

	response, err := p.v.DisassociateAddress(request)

	if err != nil {
		return 0, fmt.Errorf("[%s] calling associateAddress error, msg: %v", p.GetProviderName(), err)
	}
	if response.Response.TaskId == nil {
		return 0, nil
	}
	taskID, err := strconv.ParseUint(*response.Response.TaskId, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("[%s] error when convert taskID: %s", p.GetProviderName(), *response.Response.TaskId)
	}
	return taskID, nil
}

func (p *Tencent) describeVpcTaskResult(taskID uint64) error {
	request := vpc.NewDescribeTaskResultRequest()
	request.TaskId = tencentCommon.Uint64Ptr(taskID)

	wait.ErrWaitTimeout = fmt.Errorf("[%s] calling describeVpcTaskResult error. region: %s, zone: %s, taskId: %d",
		p.GetProviderName(), p.Region, p.Zone, taskID)

	if err := wait.ExponentialBackoff(common.Backoff, func() (bool, error) {
		response, err := p.v.DescribeTaskResult(request)
		if err != nil {
			return false, nil
		}

		switch strings.ToUpper(*response.Response.Result) {
		case tencent.Running:
			return false, nil
		case tencent.Success:
			return true, nil
		case tencent.Failed:
			return true, fmt.Errorf("[%s] task failed %d", p.GetProviderName(), taskID)
		}

		return true, nil
	}); err != nil {
		return err
	}
	return nil
}

func (p *Tencent) getInstanceStatus(aimStatus string) error {
	ids := make([]string, 0)
	p.m.Range(func(key, value interface{}) bool {
		ids = append(ids, key.(string))
		return true
	})

	if len(ids) > 0 {
		p.logger.Infof("[%s] waiting for the instances %s to be in `%s` status...", p.GetProviderName(), ids, aimStatus)
		request := cvm.NewDescribeInstancesStatusRequest()
		request.InstanceIds = tencentCommon.StringPtrs(ids)

		wait.ErrWaitTimeout = fmt.Errorf("[%s] calling getInstanceStatus error. region: %s, zone: %s, instanceName: %s, message: not `%s` status",
			p.GetProviderName(), p.Region, p.Zone, ids, aimStatus)

		if err := wait.ExponentialBackoff(common.Backoff, func() (bool, error) {
			response, err := p.c.DescribeInstancesStatus(request)
			if err != nil || len(response.Response.InstanceStatusSet) <= 0 {
				return false, nil
			}

			for _, status := range response.Response.InstanceStatusSet {
				if *status.InstanceState == aimStatus {
					instanceID := *status.InstanceId
					if value, ok := p.m.Load(instanceID); ok {
						v := value.(types.Node)
						v.InstanceStatus = aimStatus
						p.m.Store(instanceID, v)
					}
				} else {
					return false, nil
				}
			}

			return true, nil
		}); err != nil {
			return err
		}
	}

	p.logger.Infof("[%s] instances %s are in `%s` status", p.GetProviderName(), ids, aimStatus)

	return nil
}

func (p *Tencent) describeResourcesByTags() ([]*tag.ResourceTag, error) {
	request := tag.NewDescribeResourcesByTagsRequest()

	request.TagFilters = []*tag.TagFilter{
		{TagKey: tencentCommon.StringPtr("autok3s"), TagValue: tencentCommon.StringPtrs([]string{"true"})},
		{TagKey: tencentCommon.StringPtr("cluster"), TagValue: tencentCommon.StringPtrs([]string{common.TagClusterPrefix + p.Name})},
	}

	response, err := p.t.DescribeResourcesByTags(request)
	if err != nil {
		return nil, err
	}
	return response.Response.Rows, err
}

func (p *Tencent) configNetwork() error {
	// find default vpc and subnet
	request := vpc.NewDescribeVpcsRequest()

	request.Filters = []*vpc.Filter{
		{
			Values: tencentCommon.StringPtrs([]string{vpcName}),
			Name:   tencentCommon.StringPtr("vpc-name"),
		},
		{
			Name:   tencentCommon.StringPtr("tag:autok3s"),
			Values: tencentCommon.StringPtrs([]string{"true"}),
		},
	}
	response, err := p.v.DescribeVpcs(request)
	if err != nil {
		return err
	}

	if response != nil && response.Response != nil && len(response.Response.VpcSet) > 0 {
		p.logger.Infof("[%s] find existed default vpc %s for autok3s", p.GetProviderName(), vpcName)
		defaultVPC := response.Response.VpcSet[0]
		p.VpcID = *defaultVPC.VpcId
		// find default subnet
		args := vpc.NewDescribeSubnetsRequest()

		args.Filters = []*vpc.Filter{
			{
				Values: tencentCommon.StringPtrs([]string{subnetName}),
				Name:   tencentCommon.StringPtr("subnet-name"),
			},
			{
				Name:   tencentCommon.StringPtr("tag:autok3s"),
				Values: tencentCommon.StringPtrs([]string{"true"}),
			},
		}

		resp, err := p.v.DescribeSubnets(args)
		if err != nil {
			return err
		}

		if resp != nil && resp.Response != nil && len(resp.Response.SubnetSet) > 0 {
			p.logger.Infof("[%s] find existed default subnet %s for vpc %s", p.GetProviderName(), subnetName, vpcName)
			p.SubnetID = *resp.Response.SubnetSet[0].SubnetId
		} else {
			return p.generateDefaultSubnet()
		}

	} else {
		err := p.generateDefaultVPC()
		if err != nil {
			return err
		}
		err = p.generateDefaultSubnet()
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *Tencent) generateDefaultVPC() error {
	p.logger.Infof("[%s] generate default vpc %s in region %s", p.GetProviderName(), vpcName, p.Region)
	request := vpc.NewCreateVpcRequest()
	request.VpcName = tencentCommon.StringPtr(vpcName)
	request.CidrBlock = tencentCommon.StringPtr(vpcCidrBlock)
	request.Tags = []*vpc.Tag{
		{
			Key:   tencentCommon.StringPtr("autok3s"),
			Value: tencentCommon.StringPtr("true"),
		},
	}
	response, err := p.v.CreateVpc(request)
	if err != nil {
		return fmt.Errorf("[%s] fail to create default vpc %s in region %s: %v", p.GetProviderName(), vpcName, p.Region, err)
	}

	p.VpcID = *response.Response.Vpc.VpcId
	p.logger.Infof("[%s] generate default vpc %s in region %s successfully", p.GetProviderName(), vpcName, p.Region)

	return err
}

func (p *Tencent) generateDefaultSubnet() error {
	p.logger.Infof("[%s] generate default subnet %s for vpc %s in region %s", p.GetProviderName(), subnetName, vpcName, p.Region)
	request := vpc.NewCreateSubnetRequest()

	request.Tags = []*vpc.Tag{
		{
			Key:   tencentCommon.StringPtr("autok3s"),
			Value: tencentCommon.StringPtr("true"),
		},
	}
	request.VpcId = tencentCommon.StringPtr(p.VpcID)
	request.SubnetName = tencentCommon.StringPtr(subnetName)
	request.Zone = tencentCommon.StringPtr(p.Zone)
	request.CidrBlock = tencentCommon.StringPtr(subnetCidrBlock)

	response, err := p.v.CreateSubnet(request)
	if err != nil {
		return fmt.Errorf("[%s] fail to create default subnet for vpc %s in region %s, zone %s: %v", p.GetProviderName(), p.VpcID, p.Region, p.Zone, err)
	}
	p.SubnetID = *response.Response.Subnet.SubnetId
	p.logger.Infof("[%s] generate default subnet %s for vpc %s in region %s successfully", p.GetProviderName(), subnetName, vpcName, p.Region)
	return nil
}

func (p *Tencent) configSecurityGroup() error {
	p.logger.Infof("[%s] check default security group %s in region %s", p.GetProviderName(), defaultSecurityGroupName, p.Region)
	// find default security group
	request := vpc.NewDescribeSecurityGroupsRequest()

	request.Filters = []*vpc.Filter{
		{
			Values: tencentCommon.StringPtrs([]string{"true"}),
			Name:   tencentCommon.StringPtr("tag:autok3s"),
		},
		{
			Values: tencentCommon.StringPtrs([]string{defaultSecurityGroupName}),
			Name:   tencentCommon.StringPtr("security-group-name"),
		},
	}
	response, err := p.v.DescribeSecurityGroups(request)
	if err != nil {
		return err
	}

	var securityGroupID string
	if response != nil && response.Response != nil && len(response.Response.SecurityGroupSet) > 0 {
		securityGroupID = *response.Response.SecurityGroupSet[0].SecurityGroupId
		p.SecurityGroupIds = securityGroupID
	}

	if securityGroupID == "" {
		// create default security group
		p.logger.Infof("[%s] create default security group %s in region %s", p.GetProviderName(), defaultSecurityGroupName, p.Region)
		err = p.generateDefaultSecurityGroup()
		if err != nil {
			return fmt.Errorf("[%s] fail to create default security group %s: %v", p.GetProviderName(), defaultSecurityGroupName, err)
		}
	}
	err = p.configDefaultSecurityPermission()

	return err
}

func (p *Tencent) generateDefaultSecurityGroup() error {
	request := vpc.NewCreateSecurityGroupRequest()

	request.Tags = []*vpc.Tag{
		{
			Key:   tencentCommon.StringPtr("autok3s"),
			Value: tencentCommon.StringPtr("true"),
		},
	}
	request.GroupName = tencentCommon.StringPtr(defaultSecurityGroupName)
	request.GroupDescription = tencentCommon.StringPtr("generated by autok3s")

	response, err := p.v.CreateSecurityGroup(request)
	if err != nil {
		return err
	}

	p.SecurityGroupIds = *response.Response.SecurityGroup.SecurityGroupId

	return nil
}

func (p *Tencent) configDefaultSecurityPermission() error {
	p.logger.Infof("[%s] check rules of security group %s", p.GetProviderName(), defaultSecurityGroupName)
	// get security group rules
	request := vpc.NewDescribeSecurityGroupPoliciesRequest()
	request.SecurityGroupId = tencentCommon.StringPtr(p.SecurityGroupIds)
	response, err := p.v.DescribeSecurityGroupPolicies(request)
	if err != nil {
		return err
	}
	// check subnet cidr
	var cidr string
	if p.SubnetID != "" {
		cidr, err = p.getSubnetCidr()
		if err != nil {
			return err
		}
	} else {
		cidr = subnetCidrBlock
	}
	hasSSHPort := false
	hasAPIServerPort := false
	hasKubeletPort := false
	hasVXlanPort := false
	hasUIPort := false
	hasEgress := false
	hasEtcdServerPort := false
	hasEtcdPeerPort := false
	if response != nil && response.Response != nil &&
		response.Response.SecurityGroupPolicySet != nil && response.Response.SecurityGroupPolicySet.Ingress != nil {
		rules := response.Response.SecurityGroupPolicySet.Ingress
		for _, rule := range rules {
			ports := *rule.Port
			portArray := strings.Split(ports, ",")
			for _, p := range portArray {
				fromPort, _ := strconv.Atoi(p)
				switch fromPort {
				case 22:
					hasSSHPort = true
				case 6443:
					hasAPIServerPort = true
				case 10250:
					hasKubeletPort = true
				case 8472:
					hasVXlanPort = true
				case 8999:
					hasUIPort = true
				case 2379:
					if *rule.CidrBlock == cidr || *rule.CidrBlock == ipRange {
						hasEtcdServerPort = true
					}
				case 2380:
					if *rule.CidrBlock == cidr || *rule.CidrBlock == ipRange {
						hasEtcdPeerPort = true
					}
				}
			}

		}
		eRules := response.Response.SecurityGroupPolicySet.Egress
		if len(eRules) > 0 {
			hasEgress = true
		}
	}

	perms := []*vpc.SecurityGroupPolicy{}

	if !hasSSHPort {
		perms = append(perms, &vpc.SecurityGroupPolicy{
			Protocol:          tencentCommon.StringPtr("TCP"),
			Port:              tencentCommon.StringPtr("22"),
			CidrBlock:         tencentCommon.StringPtr(ipRange),
			Action:            tencentCommon.StringPtr("ACCEPT"),
			PolicyDescription: tencentCommon.StringPtr("accept for ssh(generated by autok3s)"),
		})
	}

	if (p.Network == "" || p.Network == "vxlan") && !hasVXlanPort {
		// udp 8472 for flannel vxlan
		perms = append(perms, &vpc.SecurityGroupPolicy{
			Protocol:          tencentCommon.StringPtr("UDP"),
			Port:              tencentCommon.StringPtr("8472"),
			CidrBlock:         tencentCommon.StringPtr(ipRange),
			Action:            tencentCommon.StringPtr("ACCEPT"),
			PolicyDescription: tencentCommon.StringPtr("accept for k3s vxlan(generated by autok3s)"),
		})
	}

	// port 6443 for kubernetes api-server
	if !hasAPIServerPort {
		perms = append(perms, &vpc.SecurityGroupPolicy{
			Protocol:          tencentCommon.StringPtr("TCP"),
			Port:              tencentCommon.StringPtr("6443"),
			CidrBlock:         tencentCommon.StringPtr(ipRange),
			Action:            tencentCommon.StringPtr("ACCEPT"),
			PolicyDescription: tencentCommon.StringPtr("accept for kube api-server(generated by autok3s)"),
		})
	}

	// 10250 for kubelet
	if !hasKubeletPort {
		perms = append(perms, &vpc.SecurityGroupPolicy{
			Protocol:          tencentCommon.StringPtr("TCP"),
			Port:              tencentCommon.StringPtr("10250"),
			CidrBlock:         tencentCommon.StringPtr(ipRange),
			Action:            tencentCommon.StringPtr("ACCEPT"),
			PolicyDescription: tencentCommon.StringPtr("accept for kubelet(generated by autok3s)"),
		})
	}

	if !hasEtcdServerPort || !hasEtcdPeerPort {
		perms = append(perms, &vpc.SecurityGroupPolicy{
			Protocol:          tencentCommon.StringPtr("TCP"),
			Port:              tencentCommon.StringPtr("2379"),
			CidrBlock:         tencentCommon.StringPtr(cidr),
			Action:            tencentCommon.StringPtr("ACCEPT"),
			PolicyDescription: tencentCommon.StringPtr("accept for etcd(generated by autok3s)"),
		})
		perms = append(perms, &vpc.SecurityGroupPolicy{
			Protocol:          tencentCommon.StringPtr("TCP"),
			Port:              tencentCommon.StringPtr("2380"),
			CidrBlock:         tencentCommon.StringPtr(cidr),
			Action:            tencentCommon.StringPtr("ACCEPT"),
			PolicyDescription: tencentCommon.StringPtr("accept for etcd(generated by autok3s)"),
		})
	}

	if p.UI && !hasUIPort {
		perms = append(perms, &vpc.SecurityGroupPolicy{
			Protocol:          tencentCommon.StringPtr("TCP"),
			Port:              tencentCommon.StringPtr("8999"),
			CidrBlock:         tencentCommon.StringPtr(ipRange),
			Action:            tencentCommon.StringPtr("ACCEPT"),
			PolicyDescription: tencentCommon.StringPtr("accept for dashboard(generated by autok3s)"),
		})
	}

	if len(perms) > 0 {
		args := vpc.NewCreateSecurityGroupPoliciesRequest()
		args.SecurityGroupId = tencentCommon.StringPtr(p.SecurityGroupIds)
		args.SecurityGroupPolicySet = &vpc.SecurityGroupPolicySet{
			Ingress: perms,
		}
		_, err = p.v.CreateSecurityGroupPolicies(args)
		if err != nil {
			return err
		}
	}

	// check egress
	if !hasEgress {
		args := vpc.NewCreateSecurityGroupPoliciesRequest()
		args.SecurityGroupId = tencentCommon.StringPtr(p.SecurityGroupIds)
		args.SecurityGroupPolicySet = &vpc.SecurityGroupPolicySet{
			Egress: []*vpc.SecurityGroupPolicy{
				{
					Protocol:          tencentCommon.StringPtr("ALL"),
					Port:              tencentCommon.StringPtr("all"),
					CidrBlock:         tencentCommon.StringPtr(ipRange),
					Action:            tencentCommon.StringPtr("ACCEPT"),
					PolicyDescription: tencentCommon.StringPtr("allow all egress(generated by autok3s)"),
				},
			},
		}
		_, err = p.v.CreateSecurityGroupPolicies(args)
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *Tencent) allocateEIPForInstance(num int, master bool) ([]uint64, error) {
	eipIds := []uint64{}
	eips, taskID, err := p.allocateAddresses(num)
	if err != nil {
		return nil, err
	}
	if err = p.describeVpcTaskResult(taskID); err != nil {
		p.logger.Errorf("[%s] failed to allocate eip(s) for instance(s): taskId:[%d]", p.GetProviderName(), taskID)
		return nil, err
	}
	eipAddresses, err := p.describeAddresses(eips, nil)
	if err != nil {
		p.logger.Errorf("[%s] error when query eip info:[%s]", p.GetProviderName(), tencentCommon.StringValues(eips))
		return nil, err
	}

	if eipAddresses != nil {
		p.logger.Infof("[%s] associating %d eip(s) for instance(s)", p.GetProviderName(), num)
		p.m.Range(func(key, value interface{}) bool {
			v := value.(types.Node)
			if v.Master == master && v.PublicIPAddress == nil {
				v.EipAllocationIds = append(v.EipAllocationIds, *eipAddresses[0].AddressId)
				v.PublicIPAddress = append(v.PublicIPAddress, *eipAddresses[0].AddressIp)
				taskID, err := p.associateAddress(*eipAddresses[0].AddressId, v.InstanceID)
				if err != nil {
					return false
				}
				eipIds = append(eipIds, taskID)
				eipAddresses = eipAddresses[1:]
				p.m.Store(v.InstanceID, v)
			}
			return true
		})
		p.logger.Infof("[%s] successfully associated %d eip(s) for instance(s)", p.GetProviderName(), num)
	}

	return eipIds, nil
}

func (p *Tencent) uploadKeyPair(node types.Node, publicKey string) error {
	dialer, err := hosts.SSHDialer(&hosts.Host{Node: node})
	if err != nil {
		return err
	}
	tunnel, err := dialer.OpenTunnel(true)
	if err != nil {
		return err
	}
	defer func() {
		_ = tunnel.Close()
	}()
	var (
		stdout bytes.Buffer
		stderr bytes.Buffer
	)
	command := fmt.Sprintf("mkdir -p ~/.ssh; echo '%s' > ~/.ssh/authorized_keys", strings.Trim(publicKey, "\n"))

	p.logger.Infof("[%s] upload the public key with command: %s", p.GetProviderName(), command)

	tunnel.Cmd(command)

	if err := tunnel.SetStdio(&stdout, &stderr).Run(); err != nil || stderr.String() != "" {
		return fmt.Errorf("%w: %s", err, stderr.String())
	}
	p.logger.Infof("[%s] upload keypair with output: %s", p.GetProviderName(), stdout.String())
	return nil
}

func (p *Tencent) syncNodeStatusWithInstance(ssh *types.SSH) {
	p.m.Range(func(key, value interface{}) bool {
		v := value.(types.Node)
		nodes := p.Status.WorkerNodes
		if v.Master {
			nodes = p.Status.MasterNodes
		}
		index, b := putil.IsExistedNodes(nodes, v.InstanceID)
		if !b {
			nodes = append(nodes, v)
		} else {
			node := nodes[index]
			if ssh != nil {
				if node.SSH.User == "" || node.SSH.Port == "" || (node.SSH.Password == "" && node.SSH.SSHKeyPath == "") {
					node.SSH = *ssh
				}
			}
			node.InstanceStatus = v.InstanceStatus
			nodes[index] = node
		}
		if v.Master {
			p.Status.MasterNodes = nodes
		} else {
			p.Status.WorkerNodes = nodes
		}
		return true
	})
}

func (p *Tencent) syncClusterInstance(ssh *types.SSH) ([]*cvm.Instance, error) {
	instanceList, err := p.describeInstances()
	if err != nil {
		return nil, fmt.Errorf("[%s] failed to list instance for cluster %s, region: %s, zone: %s: %v",
			p.GetProviderName(), p.Name, p.Region, p.Zone, err)
	}

	for _, instance := range instanceList {
		instanceID := *instance.InstanceId
		instanceState := *instance.InstanceState
		master := false
		for _, tagPtr := range instance.Tags {
			if strings.EqualFold(*tagPtr.Key, "master") && strings.EqualFold(*tagPtr.Value, "true") {
				master = true
				break
			}
		}
		var eip []string
		if p.PublicIPAssignedEIP {
			eipInfos, err := p.describeAddresses(nil, []*string{instance.InstanceId})
			if err != nil {
				p.logger.Errorf("[%s] error when query eip info of instance:[%s]", p.GetProviderName(), *instance.InstanceId)
				continue
			}
			for _, eipInfo := range eipInfos {
				eip = append(eip, *eipInfo.AddressId)
			}
		}
		p.m.Store(instanceID, types.Node{
			Master:            master,
			InstanceID:        instanceID,
			InstanceStatus:    instanceState,
			InternalIPAddress: tencentCommon.StringValues(instance.PrivateIpAddresses),
			PublicIPAddress:   tencentCommon.StringValues(instance.PublicIpAddresses),
			EipAllocationIds:  eip,
			SSH:               *ssh,
		})
	}
	p.syncNodeStatusWithInstance(ssh)

	return instanceList, nil
}

func (p *Tencent) getSubnetCidr() (string, error) {
	request := vpc.NewDescribeSubnetsRequest()
	request.SubnetIds = tencentCommon.StringPtrs([]string{p.SubnetID})
	response, err := p.v.DescribeSubnets(request)
	if err != nil {
		return "", err
	}

	if response != nil && response.Response != nil && response.Response.SubnetSet != nil {
		return *response.Response.SubnetSet[0].CidrBlock, nil
	}
	return "", nil
}
