package mysql

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/terraform-plugin-sdk/helper/schema"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type sessionConfig struct {
	instanceID string
	profile    string
	region     string
	sshUser    string
	keyPath    string
	localPort  uint16
	dbEndpoint string
}

func parseSessionConfig(d *schema.ResourceData) (*sessionConfig, error) {
	v, ok := d.GetOk("aws_ssm_session_manager_client_config")
	if !ok {
		return nil, nil
	}

	if !(len(v.([]interface{})) > 0 && v.([]interface{})[0] != nil) {
		return nil, fmt.Errorf("parseSessionConfig's format validate")
	}

	confMap := v.([]interface{})[0].(map[string]interface{})

	conf := &sessionConfig{}
	if v, ok := confMap["ec2_instance_id"].(string); ok && v != "" {
		conf.instanceID = v
	}

	if v, ok := confMap["rds_endpoint"].(string); ok && v != "" {
		conf.dbEndpoint = v
	}

	if v, ok := confMap["ssh_user"].(string); ok && v != "" {
		conf.sshUser = v
	}

	if v, ok := confMap["ssh_key_path"].(string); ok && v != "" {
		conf.keyPath = v
	}

	if v, ok := confMap["aws_profile"].(string); ok && v != "" {
		conf.profile = v
	}

	if v, ok := confMap["region"].(string); ok && v != "" {
		conf.region = v
	}

	if err := validateConfig(conf); err != nil {
		return nil, err
	}

	lp, _ := strconv.Atoi(strings.SplitN(d.Get("endpoint").(string), ":", 2)[1])
	conf.localPort = uint16(lp)

	return conf, nil

}

func validateConfig(conf *sessionConfig) error {

	var errors error
	if conf.instanceID == "" {
		errors = multierror.Append(errors, fmt.Errorf("not set ec2_instance_id"))
	}

	if conf.dbEndpoint == "" {
		errors = multierror.Append(errors, fmt.Errorf("not set rds_endpoint"))
	}

	if conf.sshUser == "" {
		errors = multierror.Append(errors, fmt.Errorf("not set ssh_user"))
	}

	if conf.keyPath == "" {
		errors = multierror.Append(errors, fmt.Errorf("not set ssh_key_path"))
	}

	if _, err := os.Stat(conf.keyPath); err != nil {
		errors = multierror.Append(errors, fmt.Errorf("ssh_key_path: %s is not exist", conf.keyPath))
	}

	if conf.region == "" {
		errors = multierror.Append(errors, fmt.Errorf("not set region"))
	}
	if errors != nil {
		return errors
	}
	return nil
}

func connectSession(conf *sessionConfig) error {
	if conf == nil {
		return nil
	}
	sess, err := session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable, // Must be set to enable
		Profile:           conf.profile,
		Config:            aws.Config{Region: aws.String(conf.region)},
	})
	if err != nil {
		return err
	}

	proxyCmd, closeSession, err := openSession(ssm.New(sess), conf.instanceID)
	if err != nil {
		return err
	}

	sshConfig, err := createSSHClientConfig(conf.sshUser, conf.keyPath)
	if err != nil {
		errors := err

		if err := closeSession(); err != nil {
			errors = multierror.Append(err)
		}
		closeSession()
		return errors
	}

	client, killProxyCmd, err := createSSHClientWithProxyCommand(conf.instanceID, 22, proxyCmd, sshConfig)
	if err != nil {
		errors := err

		if err := closeSession(); err != nil {
			errors = multierror.Append(err)
		}
		closeSession()
		return errors
	}

	if err := portForward(conf.localPort, client, conf.dbEndpoint); err != nil {
		errors := err

		if err := client.Close(); err != nil {
			errors = multierror.Append(err)
		}
		if err := killProxyCmd(); err != nil {
			errors = multierror.Append(err)
		}
		if err := closeSession(); err != nil {
			errors = multierror.Append(err)
		}
		return errors
	}

	return nil
}

func openSession(svc *ssm.SSM, instanceID string) (*exec.Cmd, func() error, error) {
	in := &ssm.StartSessionInput{
		DocumentName: aws.String("AWS-StartSSHSession"),
		Parameters: map[string][]*string{
			"portNumber": {aws.String("22")},
		},
		Target: aws.String(instanceID),
	}
	out, err := svc.StartSession(in)
	if err != nil {
		return nil, nil, err
	}

	close := func() error {
		in := &ssm.TerminateSessionInput{
			SessionId: out.SessionId,
		}
		if _, err := svc.TerminateSession(in); err != nil {
			return err
		}
		return nil
	}

	cmd, err := sessionManagerPlugin(svc, in, out)
	if err != nil {
		defer close()
		return nil, nil, err
	}

	return cmd, close, nil
}

func sessionManagerPlugin(
	svc *ssm.SSM,
	in *ssm.StartSessionInput,
	out *ssm.StartSessionOutput,
) (*exec.Cmd, error) {
	command := "session-manager-plugin"
	if runtime.GOOS == "windows" {
		command += ".exe"
	}

	encodedIn, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	encodedOut, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	region := *svc.Config.Region
	profile := getAWSProfile()
	endpoint := svc.Endpoint

	cmd := exec.Command(command, string(encodedOut), region,
		"StartSession", profile, string(encodedIn), endpoint)

	return cmd, nil
}

func getAWSProfile() string {
	profile := os.Getenv("AWS_PROFILE")
	if profile != "" {
		return profile
	}

	enableSharedConfig, _ := strconv.ParseBool(os.Getenv("AWS_SDK_LOAD_CONFIG"))
	if enableSharedConfig {
		profile = os.Getenv("AWS_DEFAULT_PROFILE")
	}

	return profile
}

func defaultSSHKeyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return path.Join(home, ".ssh", "id_rsa")
}

func createSSHClientConfig(user string, keyPath string) (*ssh.ClientConfig, error) {
	key, err := ioutil.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, err
	}

	hostKeyCallback, err := createHostKeyCallback()
	if err != nil {
		return nil, err
	}

	return &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: hostKeyCallback,
	}, nil
}

func createHostKeyCallback() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	knownHosts := path.Join(home, ".ssh", "known_hosts")

	cb, err := knownhosts.New(knownHosts)
	if err != nil {
		return nil, err
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if remote.String() == "pipe" {
			remote = &addrImpl{
				network: remote.Network(),
				addr:    hostname,
			}
		}

		err := cb(hostname, remote, key)

		var ke *knownhosts.KeyError
		if errors.As(err, &ke) {
			if len(ke.Want) > 0 {
				return ke
			}

			f, err := os.OpenFile(knownHosts, os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				return err
			}
			defer f.Close()

			new_host := knownhosts.Line([]string{remote.String()}, key)
			fmt.Fprintln(f, new_host)

			return nil
		}

		return err
	}, nil
}

type addrImpl struct {
	network string
	addr    string
}

func (s *addrImpl) Network() string {
	return s.network
}

func (s *addrImpl) String() string {
	return s.addr
}

func createSSHClientWithProxyCommand(
	host string,
	port uint16,
	proxyCmd *exec.Cmd,
	conf *ssh.ClientConfig,
) (*ssh.Client, func() error, error) {
	c, s := net.Pipe()

	proxyCmd.Stdin = s
	proxyCmd.Stdout = s
	proxyCmd.Stderr = os.Stderr

	if err := proxyCmd.Start(); err != nil {
		return nil, nil, err
	}

	done := func() error {
		return proxyCmd.Process.Kill()
	}

	addr := fmt.Sprintf("%s:%d", host, port)
	conn, chans, reqs, err := ssh.NewClientConn(c, addr, conf)
	if err != nil {
		defer done()
		return nil, nil, err
	}

	client := ssh.NewClient(conn, chans, reqs)

	return client, done, nil
}

func portForward(
	localPort uint16,
	sshClient *ssh.Client,
	remoteEndpoint string,
) error {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", localPort))
	if err != nil {
		return err
	}

	done := make(chan struct{})

	go func() {
		defer listener.Close()

		for {
			select {
			case <-done:
				return
			default:
			}

			localConn, err := listener.Accept()
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					continue
				}
				fmt.Fprintln(os.Stderr, "accept failed: ", err)
				return
			}

			remoteConn, err := sshClient.Dial("tcp", remoteEndpoint)
			if err != nil {
				fmt.Fprintln(os.Stderr, "dial failed: ", err)
				return
			}

			go func() {
				defer localConn.Close()
				defer remoteConn.Close()
				if _, err := io.Copy(remoteConn, localConn); err != nil {
					fmt.Fprintln(os.Stderr, "copy failed: ", err)
				}
			}()

			go func() {
				if _, err := io.Copy(localConn, remoteConn); err != nil {
					fmt.Fprintln(os.Stderr, "copy failed: ", err)
				}
			}()
		}
	}()

	return nil
}
