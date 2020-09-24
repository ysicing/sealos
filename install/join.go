package install

import (
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/fanux/sealos/cert"
	"github.com/fanux/sealos/ipvs"
	"github.com/wonderivan/logger"
)

//BuildJoin is
func BuildJoin(joinMasters, joinNodes []string) {
	if len(joinMasters) > 0 {
		joinMastersFunc(joinMasters)
	}
	if len(joinNodes) > 0 {
		joinNodesFunc(joinNodes)
	}
}

func joinMastersFunc(joinMasters []string) {
	masters := MasterIPs
	nodes := NodeIPs
	i := &SealosInstaller{
		Hosts:   joinMasters,
		Masters: masters,
		Nodes:   nodes,
	}
	i.CheckValid()
	i.SendSealos()
	i.SendPackage()
	i.GeneratorCerts()
	i.WriteNodeIP()
	i.JoinMasters(joinMasters)
	//master join to MasterIPs
	MasterIPs = append(MasterIPs, joinMasters...)
	i.lvscare()

}

//joinNodesFunc is join nodes func
func joinNodesFunc(joinNodes []string) {
	// 所有node节点
	nodes := joinNodes
	i := &SealosInstaller{
		Hosts:   nodes,
		Masters: MasterIPs,
		Nodes:   nodes,
	}
	i.CheckValid()
	i.SendSealos()
	i.SendPackage()
	i.GeneratorToken()
	i.WriteNodeIP()
	i.JoinNodes()
	//node join to NodeIPs
	NodeIPs = append(NodeIPs, joinNodes...)
}

//GeneratorToken is
//这里主要是为了获取CertificateKey
func (s *SealosInstaller) GeneratorCerts() {
	cmd := `kubeadm init phase upload-certs --upload-certs` + vlogToStr()
	output := SSHConfig.CmdToString(s.Masters[0], cmd, "\r\n")
	logger.Debug("[globals]decodeCertCmd: %s", output)
	slice := strings.Split(output, "Using certificate key:\r\n")
	slice1 := strings.Split(slice[1], "\r\n")
	CertificateKey = slice1[0]
	cmd = "kubeadm token create --print-join-command" + vlogToStr()
	out := SSHConfig.Cmd(s.Masters[0], cmd)
	decodeOutput(out)
}

//GeneratorToken is
func (s *SealosInstaller) GeneratorToken() {
	cmd := `kubeadm token create --print-join-command` + vlogToStr()
	output := SSHConfig.Cmd(s.Masters[0], cmd)
	decodeOutput(output)
}

// 返回/etc/hosts记录
func getApiserverHost(ipAddr string) (host string) {
	return fmt.Sprintf("%s %s", ipAddr, ApiServer)
}

// sendJoinCPConfig send join CP nodes configuration
func sendJoinCPConfig(joinMaster []string) {
	var wg sync.WaitGroup
	for _, master := range joinMaster {
		wg.Add(1)
		go func(master string) {
			defer wg.Done()
			templateData := string(JoinTemplate(master))
			cmd := fmt.Sprintf(`echo "%s" > /root/kubeadm-join-config.yaml`,templateData)
			_ = SSHConfig.CmdAsync(master, cmd)
		}(master)
	}
	wg.Wait()
}

//JoinMasters is
func (s *SealosInstaller) JoinMasters(masters []string) {
	var wg sync.WaitGroup
	//copy certs
	s.sendCaAndKey(masters)

	s.SendKubeConfigs(masters, false)

	// send CP nodes configuration
	sendJoinCPConfig(masters)

	//join master do sth
	cmd := s.Command(Version, JoinMaster)
	for _, master := range masters {
		wg.Add(1)
		go func(master string) {
			defer wg.Done()
			hostname := GetRemoteHostName(master)
			certCMD := cert.CertCMD(ApiServerCertSANs, IpFormat(master), hostname, SvcCIDR, DnsDomain)
			_ = SSHConfig.CmdAsync(master, certCMD)

			cmdHosts := fmt.Sprintf("echo %s >> /etc/hosts", getApiserverHost(IpFormat(s.Masters[0])))
			_ = SSHConfig.CmdAsync(master, cmdHosts)
			// cmdMult := fmt.Sprintf("%s --apiserver-advertise-address %s", cmd, IpFormat(master))
			_ = SSHConfig.CmdAsync(master, cmd)
			cmdHosts = fmt.Sprintf(`sed "s/%s/%s/g" -i /etc/hosts`, getApiserverHost(IpFormat(s.Masters[0])), getApiserverHost(IpFormat(master)))
			_ = SSHConfig.CmdAsync(master, cmdHosts)
			copyk8sConf := `mkdir -p /root/.kube && cp -i /etc/kubernetes/admin.conf /root/.kube/config`
			_ = SSHConfig.CmdAsync(master, copyk8sConf)
			cleaninstall := `rm -rf /root/kube || :`
			_ = SSHConfig.CmdAsync(master, cleaninstall)
		}(master)
	}
	wg.Wait()
}

//JoinNodes is
func (s *SealosInstaller) JoinNodes() {
	var masters string
	var wg sync.WaitGroup
	for _, master := range s.Masters {
		masters += fmt.Sprintf(" --rs %s:6443", IpFormat(master))
	}
	ipvsCmd := fmt.Sprintf("sealos ipvs --vs %s:6443 %s --health-path /healthz --health-schem https --run-once", VIP, masters)

	for _, node := range s.Nodes {
		wg.Add(1)
		go func(node string) {
			defer wg.Done()
			cmdHosts := fmt.Sprintf("echo %s %s >> /etc/hosts", VIP, ApiServer)
			_ = SSHConfig.CmdAsync(node, cmdHosts)

			// 如果不是默认路由， 则添加 vip 到 master的路由。
			cmdRoute := fmt.Sprintf("sealos route --host %s", IpFormat(node))
			status := SSHConfig.CmdToString(node, cmdRoute, "")
			if status != "ok" {
				// 以自己的ip作为路由网关
				addRouteCmd := fmt.Sprintf("sealos route add --host %s --gateway %s", VIP, IpFormat(node))
				SSHConfig.CmdToString(node, addRouteCmd, "")
			}

			_ = SSHConfig.CmdAsync(node, ipvsCmd) // create ipvs rules before we join node
			cmd := s.Command(Version, JoinNode)
			//create lvscare static pod
			yaml := ipvs.LvsStaticPodYaml(VIP, MasterIPs, LvscareImage)
			_ = SSHConfig.CmdAsync(node, cmd)
			_ = SSHConfig.Cmd(node, "mkdir -p /etc/kubernetes/manifests")
			SSHConfig.CopyConfigFile(node, "/etc/kubernetes/manifests/kube-sealyun-lvscare.yaml", []byte(yaml))

			cleaninstall := `rm -rf /root/kube`
			_ = SSHConfig.CmdAsync(node, cleaninstall)
		}(node)
	}

	wg.Wait()
}

func (s *SealosInstaller) lvscare() {
	var wg sync.WaitGroup
	for _, node := range s.Nodes {
		wg.Add(1)
		go func(node string) {
			defer wg.Done()
			yaml := ipvs.LvsStaticPodYaml(VIP, MasterIPs, LvscareImage)
			_ = SSHConfig.Cmd(node, "rm -rf  /etc/kubernetes/manifests/kube-sealyun-lvscare* || :")
			SSHConfig.CopyConfigFile(node, "/etc/kubernetes/manifests/kube-sealyun-lvscare.yaml", []byte(yaml))
		}(node)
	}

	wg.Wait()
}

func (s *SealosInstaller) sendCaAndKey(hosts []string) {
	//cert generator in sealos
	caConfigs := cert.CaList(CertPath, CertEtcdPath)
	SendPackage(CertPath+"/sa.key", hosts, cert.KubeDefaultCertPath, nil, nil)
	SendPackage(CertPath+"/sa.pub", hosts, cert.KubeDefaultCertPath, nil, nil)
	for _, ca := range caConfigs {
		SendPackage(path.Join(ca.Path, ca.BaseName+".key"), hosts, ca.DefaultPath, nil, nil)
		SendPackage(path.Join(ca.Path, ca.BaseName+".crt"), hosts, ca.DefaultPath, nil, nil)
	}
}

func (s *SealosInstaller) sendCerts(hosts []string) {
	certConfigs := cert.CertList(CertPath, CertEtcdPath)
	for _, cert := range certConfigs {
		SendPackage(path.Join(cert.Path, cert.BaseName+".key"), hosts, cert.DefaultPath, nil, nil)
		SendPackage(path.Join(cert.Path, cert.BaseName+".crt"), hosts, cert.DefaultPath, nil, nil)
	}
}

func (s *SealosInstaller) WriteNodeIP() {
	for _, ip := range s.Hosts {
		logger.Debug("node ip: ", strings.Split(ip, ":")[0])
		ext := fmt.Sprintf(`echo 'KUBELET_EXTRA_ARGS="--node-ip=%v"' > /etc/default/kubelet`, strings.Split(ip, ":")[0])
		SSHConfig.Cmd(ip, ext)
	}
}
