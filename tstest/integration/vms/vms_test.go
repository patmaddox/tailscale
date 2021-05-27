// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build linux

package vms

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"text/template"
	"time"

	expect "github.com/google/goexpect"
	"golang.org/x/crypto/ssh"
	"tailscale.com/tstest/integration/testcontrol"
)

var runVMTests = flag.Bool("run-vm-tests", false, "if set, run expensive (10G+ ram) VM based integration tests")

type Distro struct {
	name           string // amazon-linux
	size           string // 5
	url            string // URL to a qcow2 image
	sha256sum      string // hex-encoded sha256 sum
	mem            int    // VM memory in megabytes
	packageManager string // yum/apt/dnf/zypper
}

func (d *Distro) InstallPre() string {
	switch d.packageManager {
	case "yum":
		return ` - [ yum, update, gnupg2 ]
`
	case "apt":
		return ` - [ apt-get, update ]
 - [ apt-get, "-y", install, curl, "apt-transport-https", gnupg2 ]
`
	}

	return ""
}

// fetchDistro fetches a distribution from the internet if it doesn't already exist locally. It
// also validates the sha256 sum from a known good hash.
func fetchDistro(t *testing.T, resultDistro Distro) {
	t.Helper()

	cdir, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("can't find cache dir: %v", err)
	}
	cdir = filepath.Join(cdir, "tailscale", "vm-test")

	qcowPath := filepath.Join(cdir, "qcow2", resultDistro.sha256sum)

	_, err = os.Stat(qcowPath)
	if err != nil {
		t.Logf("downloading distro image %s to %s", resultDistro.url, qcowPath)
		fout, err := os.Create(qcowPath)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(resultDistro.url)
		if err != nil {
			t.Fatalf("can't fetch qcow2 for %s (%s): %v", resultDistro.name, resultDistro.url, err)
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("%s replied %s", resultDistro.url, resp.Status)
		}

		_, err = io.Copy(fout, resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("download of %s failed: %v", resultDistro.url, err)
		}

		err = fout.Close()
		if err != nil {
			t.Fatalf("can't close fout: %v", err)
		}

		fin, err := os.Open(qcowPath)
		if err != nil {
			t.Fatal(err)
		}

		hasher := sha256.New()
		if _, err := io.Copy(hasher, fin); err != nil {
			t.Fatal(err)
		}
		hash := hex.EncodeToString(hasher.Sum(nil))

		if hash != resultDistro.sha256sum {
			t.Logf("want: %q", resultDistro.sha256sum)
			t.Logf("got:  %q", hash)
			t.Fatal("hash mismatch, someone is doing something nasty")
		}

		t.Logf("hash check passed (%s)", resultDistro.sha256sum)
	}
}

// run runs a command or fails the test.
func run(t *testing.T, dir, prog string, args ...string) {
	t.Helper()
	t.Logf("running: %s %s", prog, strings.Join(args, " "))

	cmd := exec.Command(prog, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}

// mkLayeredQcow makes a layered qcow image that allows us to keep the upstream VM images
// pristine and only do our changes on an overlay.
func mkLayeredQcow(t *testing.T, tdir string, d Distro) {
	t.Helper()

	cdir, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("can't find cache dir: %v", err)
	}
	cdir = filepath.Join(cdir, "tailscale", "vm-test")

	run(t, tdir, "qemu-img", "create",
		"-f", "qcow2",
		"-o", "backing_file="+filepath.Join(cdir, "qcow2", d.sha256sum),
		filepath.Join(tdir, d.name+".qcow2"),
	)
}

// mkSeed makes the cloud-init seed ISO that is used to configure a VM with tailscale.
func mkSeed(t *testing.T, d Distro, sshKey, hostURL, tdir string, port int) {
	t.Helper()

	dir := filepath.Join(tdir, d.name, "seed")
	os.MkdirAll(dir, 0700)

	// make meta-data
	{
		fout, err := os.Create(filepath.Join(dir, "meta-data"))
		if err != nil {
			t.Fatal(err)
		}

		err = template.Must(template.New("meta-data.yaml").Parse(metaDataTemplate)).Execute(fout, struct {
			ID       string
			Hostname string
		}{
			ID:       "31337",
			Hostname: d.name,
		})
		if err != nil {
			t.Fatal(err)
		}

		fout.Close()
	}

	// make user-data
	{
		fout, err := os.Create(filepath.Join(dir, "user-data"))
		if err != nil {
			t.Fatal(err)
		}

		err = template.Must(template.New("user-data.yaml").Parse(userDataTemplate)).Execute(fout, struct {
			SSHKey     string
			HostURL    string
			Hostname   string
			Port       int
			InstallPre string
		}{
			SSHKey:     strings.TrimSpace(sshKey),
			HostURL:    hostURL,
			Hostname:   d.name,
			Port:       port,
			InstallPre: d.InstallPre(),
		})
		if err != nil {
			t.Fatal(err)
		}

		fout.Close()
	}

	run(t, tdir, "genisoimage",
		"-output", filepath.Join(dir, "seed.iso"),
		"-volid", "cidata", "-joliet", "-rock",
		filepath.Join(dir, "meta-data"),
		filepath.Join(dir, "user-data"),
	)
}

// mkVM makes a KVM-accelerated virtual machine and prepares it for introduction to the
// testcontrol server. The function it returns is for killing the virtual machine when it
// is time for it to die.
func mkVM(t *testing.T, n int, d Distro, sshKey, hostURL, tdir string) func() {
	t.Helper()

	cdir, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("can't find cache dir: %v", err)
	}
	cdir = filepath.Join(cdir, "within", "mkvm")
	os.MkdirAll(filepath.Join(cdir, "qcow2"), 0755)
	os.MkdirAll(filepath.Join(cdir, "seed"), 0755)

	port := 23100 + n

	fetchDistro(t, d)
	mkLayeredQcow(t, tdir, d)
	mkSeed(t, d, sshKey, hostURL, tdir, port)

	driveArg := fmt.Sprintf("file=%s,if=virtio", filepath.Join(tdir, d.name+".qcow2"))

	args := []string{
		"-machine", "pc-q35-5.1,accel=kvm,usb=off,vmport=off,dump-guest-core=off",
		"-netdev", fmt.Sprintf("user,hostfwd=::%d-:22,id=net0", port),
		"-device", "virtio-net-pci,netdev=net0,id=net0,mac=8a:28:5c:30:1f:25",
		"-m", fmt.Sprint(d.mem),
		"-boot", "c",
		"-drive", driveArg,
		"-cdrom", filepath.Join(tdir, d.name, "seed", "seed.iso"),
		"-vnc", fmt.Sprintf(":%d", n),
	}

	t.Logf("running: qemu-system-x86_64 %s", strings.Join(args, " "))

	cmd := exec.Command("qemu-system-x86_64", args...)
	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Second)

	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("qemu is not running")
	}

	return func() {
		err := cmd.Process.Kill()
		if err != nil {
			t.Errorf("can't kill %s (%d): %v", d.name, cmd.Process.Pid, err)
		}
	}
}

// TestVMIntegrationEndToEnd creates a virtual machine with mkvm(1X), installs tailscale on it and then ensures that it connects to the network successfully.
func TestVMIntegrationEndToEnd(t *testing.T) {
	if !*runVMTests {
		t.Skip("not running integration tests (need -run-vm-tests)")
	}

	if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
		t.Logf("hint: nix-shell -p go -p qemu -p cdrkit --run 'go test -v -timeout=60m -run-vm-tests'")
		t.Fatalf("missing dependency: %v", err)
	}

	if _, err := exec.LookPath("genisoimage"); err != nil {
		t.Logf("hint: nix-shell -p go -p qemu -p cdrkit --run 'go test -v -timeout=60m -run-vm-tests'")
		t.Fatalf("missing dependency: %v", err)
	}

	distros := []Distro{
		{"amazon-linux", "25", "https://cdn.amazonlinux.com/os-images/2.0.20210427.0/kvm/amzn2-kvm-2.0.20210427.0-x86_64.xfs.gpt.qcow2", "6ef9daef32cec69b2d0088626ec96410cd24afc504d57278bbf2f2ba2b7e529b", 512, "yum"},
		{"centos-7", "8", "https://cloud.centos.org/centos/7/images/CentOS-7-x86_64-GenericCloud.qcow2", "1db30c9c272fb37b00111b93dcebff16c278384755bdbe158559e9c240b73b80", 512, "yum"},
		{"centos-8", "10", "https://cloud.centos.org/centos/8/x86_64/images/CentOS-8-GenericCloud-8.3.2011-20201204.2.x86_64.qcow2", "7ec97062618dc0a7ebf211864abf63629da1f325578868579ee70c495bed3ba0", 768, "dnf"},
		{"debian-9", "5", "https://cdimage.debian.org/cdimage/openstack/9.13.21-20210511/debian-9.13.21-20210511-openstack-amd64.qcow2", "0667a08e2d947b331aee068db4bbf3a703e03edaf5afa52e23d534adff44b62a", 512, "apt"},
		{"debian-10", "5", "https://cdimage.debian.org/images/cloud/buster/20210329-591/debian-10-generic-amd64-20210329-591.qcow2", "70c61956095870c4082103d1a7a1cb5925293f8405fc6cb348588ec97e8611b0", 768, "apt"},
		{"fedora-34", "5", "https://download.fedoraproject.org/pub/fedora/linux/releases/34/Cloud/x86_64/images/Fedora-Cloud-Base-34-1.2.x86_64.qcow2", "b9b621b26725ba95442d9a56cbaa054784e0779a9522ec6eafff07c6e6f717ea", 768, "dnf"},
		{"opensuse-leap-15.1", "10", "https://download.opensuse.org/repositories/Cloud:/Images:/Leap_15.1/images/openSUSE-Leap-15.1-OpenStack.x86_64.qcow2", "3203e256dab5981ca3301408574b63bc522a69972fbe9850b65b54ff44a96e0a", 512, "zypper"},
		{"opensuse-leap-15.2", "10", "https://download.opensuse.org/repositories/Cloud:/Images:/Leap_15.2/images/openSUSE-Leap-15.2-OpenStack.x86_64.qcow2", "4df9cee9281d1f57d20f79dc65d76e255592b904760e73c0dd44ac753a54330f", 512, "zypper"},
		{"opensuse-tumbleweed", "5", "https://download.opensuse.org/tumbleweed/appliances/openSUSE-Tumbleweed-JeOS.x86_64-OpenStack-Cloud.qcow2", "f5a40c693187dddc524b0877b0aba31cdb6f2e78f12acce0bbb2975ec120dfc0", 512, "zypper"},
		{"ubuntu-16-04", "5", "https://cloud-images.ubuntu.com/xenial/current/xenial-server-cloudimg-amd64-disk1.img", "50a21bc067c05e0c73bf5d8727ab61152340d93073b3dc32eff18b626f7d813b", 512, "apt"},
		{"ubuntu-18-04", "5", "https://cloud-images.ubuntu.com/bionic/current/bionic-server-cloudimg-amd64.img", "78f09b6086367914d23f26e38c3cd88a2aaf1a6a4500ebd46c74ad60c56c1585", 512, "apt"},
		{"ubuntu-20-04", "5", "https://cloud-images.ubuntu.com/focal/current/focal-server-cloudimg-amd64.img", "513158b22ff0f08d0a078d8d60293bcddffdb17094a7809c76c52aba415ecc54", 512, "apt"},
		{"ubuntu-20-10", "5", "https://cloud-images.ubuntu.com/groovy/current/groovy-server-cloudimg-amd64.img", "c1332c24557389a129ff98fa169e34cb53c02555ed702a235e26b8978dd004c3", 512, "apt"},
		{"ubuntu-21-04", "5", "https://cloud-images.ubuntu.com/hirsute/current/hirsute-server-cloudimg-amd64.img", "2f8a562637340a026f712594f1257673543d74725d8e3daf88d533d7b8bf448f", 512, "apt"},
	}

	dir := t.TempDir()

	ln, err := net.Listen("tcp", "192.168.122.1:0")
	if err != nil {
		t.Fatalf("can't make TCP listener: %v", err)
	}
	defer ln.Close()
	t.Logf("host:port: %s", ln.Addr())

	cs := &testcontrol.Server{}

	var (
		ipMu  sync.Mutex
		ipMap = map[string]string{} // SSH port => IP address
	)

	mux := http.NewServeMux()
	mux.Handle("/", cs)

	// This handler will let the virtual machines tell the host information about that VM.
	// This is used to maintain a list of port->IP address mappings that are known to be
	// working. This allows later steps to connect over SSH. This returns no response to
	// clients because no response is needed.
	mux.HandleFunc("/myip/", func(w http.ResponseWriter, r *http.Request) {
		ipMu.Lock()
		defer ipMu.Unlock()

		name := path.Base(r.URL.Path)
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		ipMap[name] = host
		t.Logf("%s: %v", name, host)
	})

	hs := &http.Server{Handler: mux}
	go hs.Serve(ln)

	run(t, dir, "ssh-keygen", "-t", "ed25519", "-f", "machinekey", "-N", ``)
	pubkey, err := os.ReadFile(filepath.Join(dir, "machinekey.pub"))
	if err != nil {
		t.Fatalf("can't read ssh key: %v", err)
	}

	privateKey, err := os.ReadFile(filepath.Join(dir, "machinekey"))
	if err != nil {
		t.Fatalf("can't read ssh private key: %v", err)
	}

	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		t.Fatalf("can't parse private key: %v", err)
	}

	loginServer := fmt.Sprintf("http://%s", ln.Addr())

	cancels := make(chan func(), len(distros))

	t.Run("mkvm", func(t *testing.T) {
		for n, distro := range distros {
			n, distro := n, distro
			t.Run(distro.name, func(t *testing.T) {
				t.Parallel()

				cancel := mkVM(t, n, distro, string(pubkey), loginServer, dir)
				cancels <- cancel
			})
		}
	})

	close(cancels)
	for cancel := range cancels {
		//lint:ignore SA9001 They do actually get ran
		defer cancel()

		if len(cancels) == 0 {
			t.Log("all VMs started")
			break
		}
	}

	t.Run("wait-for-vms", func(t *testing.T) {
		t.Log("waiting for VMs to register")
		waiter := time.NewTicker(time.Second)
		defer waiter.Stop()
		n := 0
		for {
			<-waiter.C
			ipMu.Lock()
			if len(ipMap) == len(distros) {
				ipMu.Unlock()
				break
			} else {
				if n%30 == 0 {
					t.Logf("ipMap:   %d", len(ipMap))
					t.Logf("distros: %d", len(distros))
				}
			}
			n++
			ipMu.Unlock()
		}
	})

	ipMu.Lock()
	defer ipMu.Unlock()
	t.Run("join-net", func(t *testing.T) {
		for port := range ipMap {
			port := port
			t.Run(port, func(t *testing.T) {
				config := &ssh.ClientConfig{
					User:            "ts",
					Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer), ssh.Password("hunter2")},
					HostKeyCallback: ssh.InsecureIgnoreHostKey(),
				}

				cli, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%s", port), config)
				if err != nil {
					t.Fatalf("can't dial 127.0.0.1:%s: %v", port, err)
				}
				defer cli.Close()

				t.Parallel()
				t.Logf("about to ssh into 127.0.0.1:%s", port)
				timeout := 5 * time.Minute

				e, _, err := expect.SpawnSSH(cli, timeout, expect.Verbose(true), expect.VerboseWriter(os.Stdout))
				if err != nil {
					t.Fatalf("%s: can't register a shell session: %v", port, err)
				}
				defer e.Close()

				_, _, err = e.Expect(regexp.MustCompile(`(\$|\>)`), timeout)
				if err != nil {
					t.Fatalf("%s: can't get a shell: %v", port, err)
				}
				t.Logf("got shell for %s", port)
				err = e.Send(fmt.Sprintf("sudo tailscale up --login-server %s\n", loginServer))
				if err != nil {
					t.Fatalf("%s: can't send tailscale up command: %v", port, err)
				}
				_, _, err = e.Expect(regexp.MustCompile(`Success.`), timeout)
				if err != nil {
					t.Fatalf("can't extract URL: %v", err)
				}
			})
		}
	})

	if numNodes := cs.NumNodes(); numNodes != len(ipMap) {
		t.Errorf("wanted %d nodes, got: %d", len(ipMap), numNodes)
	}
}

const metaDataTemplate = `instance-id: {{.ID}}
local-hostname: {{.Hostname}}`

const userDataTemplate = `#cloud-config
#vim:syntax=yaml

cloud_config_modules:
 - runcmd

cloud_final_modules:
 - [users-groups, always]
 - [scripts-user, once-per-instance]

users:
  - name: ts
    plain_text_passwd: hunter2
    groups: [ wheel ]
    sudo: [ "ALL=(ALL) NOPASSWD:ALL" ]
    shell: /bin/sh
    ssh-authorized-keys:
    - {{.SSHKey}}

write_files:
  - path: /etc/cloud/cloud.cfg.d/80_disable_network_after_firstboot.cfg
    content: |
      # Disable network configuration after first boot
      network:
        config: disabled

runcmd:
{{.InstallPre}}
 - [ "sh", "-c", "curl https://raw.githubusercontent.com/tailscale/tailscale/Xe/test-install-script-libvirtd/scripts/installer.sh | sh" ]
 - [ systemctl, enable, --now, tailscaled.service ]
 - [ curl, "{{.HostURL}}/myip/{{.Port}}", "-H", "User-Agent: {{.Hostname}}" ]
`