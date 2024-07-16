package mysql

import (
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/hashicorp/go-version"
	"github.com/moto-taka/terraform-provider-mysql/mysql/port_forward"

	"github.com/hashicorp/terraform-plugin-sdk/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/helper/validation"
	"github.com/hashicorp/terraform-plugin-sdk/terraform"

	"golang.org/x/net/proxy"
)

const (
	cleartextPasswords = "cleartext"
	nativePasswords    = "native"
)

type MySQLConfiguration struct {
	Config          *mysql.Config
	MaxConnLifetime time.Duration
	MaxOpenConns    int
}

func Provider() terraform.ResourceProvider {
	return &schema.Provider{
		Schema: map[string]*schema.Schema{
			"endpoint": {
				Type:        schema.TypeString,
				Required:    true,
				DefaultFunc: schema.EnvDefaultFunc("MYSQL_ENDPOINT", nil),
				ValidateFunc: func(v interface{}, k string) (ws []string, errors []error) {
					value := v.(string)
					if value == "" {
						errors = append(errors, fmt.Errorf("Endpoint must not be an empty string"))
					}

					return
				},
			},

			"username": {
				Type:        schema.TypeString,
				Required:    true,
				DefaultFunc: schema.EnvDefaultFunc("MYSQL_USERNAME", nil),
			},

			"password": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("MYSQL_PASSWORD", nil),
			},

			"proxy": {
				Type:     schema.TypeString,
				Optional: true,
				DefaultFunc: schema.MultiEnvDefaultFunc([]string{
					"ALL_PROXY",
					"all_proxy",
				}, nil),
				ValidateFunc: validation.StringMatch(regexp.MustCompile("^socks5h?://.*:\\d+$"), "The proxy URL is not a valid socks url."),
			},

			"tls": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("MYSQL_TLS_CONFIG", "false"),
				ValidateFunc: validation.StringInSlice([]string{
					"true",
					"false",
					"skip-verify",
				}, false),
			},

			"max_conn_lifetime_sec": {
				Type:     schema.TypeInt,
				Optional: true,
			},

			"max_open_conns": {
				Type:     schema.TypeInt,
				Optional: true,
			},

			"authentication_plugin": {
				Type:         schema.TypeString,
				Optional:     true,
				Default:      nativePasswords,
				ValidateFunc: validation.StringInSlice([]string{cleartextPasswords, nativePasswords}, true),
			},

			"aws_ssm_session_manager_client_config": {
				Type:        schema.TypeList,
				Optional:    true,
				MaxItems:    1,
				Description: "Configuration for use aws sesion manager.",
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"ec2_instance_id": {
							Type:     schema.TypeString,
							Required: true,
						},
						"rds_endpoint": {
							Type:     schema.TypeString,
							Required: true,
						},
						"use_remote_port_forward": {
							Type:     schema.TypeBool,
							Optional: true,
							Default:  true,
						},
						"ssh_user": {
							Type:     schema.TypeString,
							Optional: true,
						},
						"ssh_key_path": {
							Type:     schema.TypeString,
							Optional: true,
						},
						"aws_profile": {
							Type: schema.TypeString,
							DefaultFunc: schema.MultiEnvDefaultFunc([]string{
								"AWS_PROFILE",
								"AWS_DEFAULT_PROFILE",
							}, ""),
							Optional: true,
						},
						"region": {
							Type: schema.TypeString,
							DefaultFunc: schema.MultiEnvDefaultFunc([]string{
								"AWS_REGION",
								"AWS_DEFAULT_REGION",
							}, ""),
							Optional: true,
						},
					},
				},
			},
			"port_forward_client_config": {
				Type:        schema.TypeList,
				Optional:    true,
				MaxItems:    1,
				Description: "Configuration for Port Forward.",
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"remote_host": {
							Type:     schema.TypeString,
							Required: true,
						},
						"db_endpoint": {
							Type:     schema.TypeString,
							Required: true,
						},
						"ssh_user": {
							Type:     schema.TypeString,
							Optional: true,
						},
						"ssh_key_path": {
							Type:     schema.TypeString,
							Optional: true,
						},
					},
				},
			},
		},

		ResourcesMap: map[string]*schema.Resource{
			"mysql_database":      resourceDatabase(),
			"mysql_grant":         resourceGrant(),
			"mysql_role":          resourceRole(),
			"mysql_user":          resourceUser(),
			"mysql_user_password": resourceUserPassword(),
		},

		ConfigureFunc: providerConfigure,
	}
}

func providerConfigure(d *schema.ResourceData) (interface{}, error) {

	var endpoint = d.Get("endpoint").(string)

	proto := "tcp"
	if len(endpoint) > 0 && endpoint[0] == '/' {
		proto = "unix"
	}

	conf := mysql.Config{
		User:                    d.Get("username").(string),
		Passwd:                  d.Get("password").(string),
		Net:                     proto,
		Addr:                    endpoint,
		TLSConfig:               d.Get("tls").(string),
		AllowNativePasswords:    d.Get("authentication_plugin").(string) == nativePasswords,
		AllowCleartextPasswords: d.Get("authentication_plugin").(string) == cleartextPasswords,
	}

	dialer, err := makeDialer(d)
	if err != nil {
		return nil, err
	}

	mysql.RegisterDial("tcp", func(network string) (net.Conn, error) {
		return dialer.Dial("tcp", network)
	})

	sessionConf, pfConfMap, err := port_forward.ParseSessionConfig(d)
	if err != nil {
		return nil, err
	}
	if pfConfMap == nil {
		pfConfMap, err = port_forward.ParsePFConfigMap(d)
		if err != nil {
			return nil, err
		}
	}
	lp, _ := strconv.Atoi(strings.SplitN(d.Get("endpoint").(string), ":", 2)[1])
	pfConf, err := port_forward.ParsePFConfig(pfConfMap, uint16(lp))
	if err != nil {
		return nil, err
	}

	if err := port_forward.Connect(sessionConf, pfConf); err != nil {
		return nil, err
	}

	return &MySQLConfiguration{
		Config:          &conf,
		MaxConnLifetime: time.Duration(d.Get("max_conn_lifetime_sec").(int)) * time.Second,
		MaxOpenConns:    d.Get("max_open_conns").(int),
	}, nil
}

var identQuoteReplacer = strings.NewReplacer("`", "``")

func makeDialer(d *schema.ResourceData) (proxy.Dialer, error) {
	proxyFromEnv := proxy.FromEnvironment()
	proxyArg := d.Get("proxy").(string)

	if len(proxyArg) > 0 {
		proxyURL, err := url.Parse(proxyArg)
		if err != nil {
			return nil, err
		}
		proxy, err := proxy.FromURL(proxyURL, proxyFromEnv)
		if err != nil {
			return nil, err
		}

		return proxy, nil
	}

	return proxyFromEnv, nil
}

func quoteIdentifier(in string) string {
	return fmt.Sprintf("`%s`", identQuoteReplacer.Replace(in))
}

func serverVersion(db *sql.DB) (*version.Version, error) {
	var versionString string
	err := db.QueryRow("SELECT @@GLOBAL.innodb_version").Scan(&versionString)
	if err != nil {
		return nil, err
	}

	return version.NewVersion(versionString)
}

func serverVersionString(db *sql.DB) (string, error) {
	var versionString string
	err := db.QueryRow("SELECT @@GLOBAL.version").Scan(&versionString)
	if err != nil {
		return "", err
	}

	return versionString, nil
}

func connectToMySQL(conf *MySQLConfiguration) (*sql.DB, error) {

	dsn := conf.Config.FormatDSN()
	var db *sql.DB
	var err error

	// When provisioning a database server there can often be a lag between
	// when Terraform thinks it's available and when it is actually available.
	// This is particularly acute when provisioning a server and then immediately
	// trying to provision a database on it.
	retryError := resource.Retry(5*time.Minute, func() *resource.RetryError {
		db, err = sql.Open("mysql", dsn)
		if err != nil {
			return resource.RetryableError(err)
		}

		err = db.Ping()
		if err != nil {
			return resource.RetryableError(err)
		}

		return nil
	})

	if retryError != nil {
		return nil, fmt.Errorf("Could not connect to server: %s", retryError)
	}
	db.SetConnMaxLifetime(conf.MaxConnLifetime)
	db.SetMaxOpenConns(conf.MaxOpenConns)
	return db, nil
}
