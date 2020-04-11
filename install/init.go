package install

import (
	"fmt"
	"github.com/fanux/sealos/cert"
	"github.com/fanux/sealos/net"
	"github.com/wonderivan/logger"
	"io/ioutil"
	"os"
)

//BuildInit is
func BuildInit() {
	MasterIPs = ParseIPs(MasterIPs)
	NodeIPs = ParseIPs(NodeIPs)
	// 所有master节点
	masters := MasterIPs
	// 所有node节点
	nodes := NodeIPs
	hosts := append(masters, nodes...)
	i := &SealosInstaller{
		Hosts:   hosts,
		Masters: masters,
		Nodes:   nodes,
	}
	i.CheckValid()
	i.Print()
	i.SendPackage()
	i.Print("SendPackage")
	i.KubeadmConfigInstall()
	i.Print("SendPackage", "KubeadmConfigInstall")
	i.GenerateCert()
	i.InstallMaster0()
	i.Print("SendPackage", "KubeadmConfigInstall", "InstallMaster0")
	if len(masters) > 1 {
		i.JoinMasters(i.Masters[1:])
		i.Print("SendPackage", "KubeadmConfigInstall", "InstallMaster0", "JoinMasters")
	}
	if len(nodes) > 0 {
		i.JoinNodes()
		i.Print("SendPackage", "KubeadmConfigInstall", "InstallMaster0", "JoinMasters", "JoinNodes")
	}
	i.PrintFinish()
}

//KubeadmConfigInstall is
func (s *SealosInstaller) KubeadmConfigInstall() {
	var templateData string
	if KubeadmFile == "" {
		templateData = string(Template())
	} else {
		fileData, err := ioutil.ReadFile(KubeadmFile)
		defer func() {
			if r := recover(); r != nil {
				logger.Error("[globals]template file read failed:", err)
			}
		}()
		if err != nil {
			panic(1)
		}
		templateData = string(TemplateFromTemplateContent(string(fileData)))
	}
	cmd := "echo \"" + templateData + "\" > /root/kubeadm-config.yaml"
	_ = SSHConfig.CmdAsync(s.Masters[0], cmd)
	//读取模板数据
	kubeadm := KubeadmDataFromYaml(templateData)
	if kubeadm != nil {
		DnsDomain = kubeadm.Networking.DnsDomain
		ApiServerCertSANs = kubeadm.ApiServer.CertSANs
	} else {
		logger.Warn("decode certSANs from config failed, using default SANs")
		ApiServerCertSANs = getDefaultSANs()
	}
}

func getDefaultSANs() []string {
	var sans = []string{"127.0.0.1", "apiserver.cluster.local", VIP}
	for _, master := range MasterIPs {
		sans = append(sans, IpFormat(master))
	}
	return sans
}

func (s *SealosInstaller) GenerateCert() {
	//cert generator in sealos
	hostname := GetRemoteHostName(s.Masters[0])
	cert.GenerateCert(CertPath, CertEtcdPath, ApiServerCertSANs, IpFormat(s.Masters[0]), hostname, SvcCIDR, DnsDomain)
	//copy all cert to master0
	//CertSA(kye,pub) + CertCA(key,crt)
	s.sendCaAndKey([]string{s.Masters[0]})
	s.sendCerts([]string{s.Masters[0]})
}

//InstallMaster0 is
func (s *SealosInstaller) InstallMaster0() {
	//master0 do sth
	cmd := fmt.Sprintf("echo %s %s >> /etc/hosts", IpFormat(s.Masters[0]), ApiServer)
	_ = SSHConfig.CmdAsync(s.Masters[0], cmd)

	cmd = s.Command(Version, InitMaster)

	output := SSHConfig.Cmd(s.Masters[0], cmd)
	if output == nil {
		logger.Error("[%s]kubernetes install is error.please clean and uninstall.", s.Masters[0])
		os.Exit(1)
	}
	decodeOutput(output)

	cmd = `mkdir -p /root/.kube && cp /etc/kubernetes/admin.conf /root/.kube/config`
	output = SSHConfig.Cmd(s.Masters[0], cmd)

	if WithoutCNI {
		logger.Info("--without-cni is true, so we not install calico or flannel, install it by yourself")
		return
	}
	//cmd = `kubectl apply -f /root/kube/conf/net/calico.yaml || true`
	netyaml := net.NewNetwork(Network, net.MetaData{
		Interface: Interface,
		CIDR:      PodCIDR,
		IPIP:      IPIP,
		MTU:       MTU,
	}).Manifests("")

	cmd = fmt.Sprintf(`echo '%s' | kubectl apply -f -`, netyaml)
	output = SSHConfig.Cmd(s.Masters[0], cmd)
}
