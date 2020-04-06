# Keymaster
[![Build Status](https://travis-ci.org/Cloud-Foundations/keymaster.svg?branch=master)](https://travis-ci.org/Cloud-Foundations/keymaster)
[![Coverage Status](https://coveralls.io/repos/github/Cloud-Foundations/keymaster/badge.svg?branch=master)](https://coveralls.io/github/Cloud-Foundations/keymaster?branch=master)
[![Go Report Card](https://goreportcard.com/badge/github.com/Cloud-Foundations/keymaster)](https://goreportcard.com/report/github.com/Cloud-Foundations/keymaster)

Keymaster is usable short-term certificate based identity system. With a primary goal to be a single-sign-on (with optional second factor with [Symantec VIP](https://vip.symantec.com/) or [U2F](https://fidoalliance.org/specifications/overview/) tokens) for CLI operations (both SSHD and TLS).

This system is easy to use, configure and administer.
Keymaster has the following components:
* `keymasterd` is the server-side daemon that runs the management interface, logs web interface and the functionality which generates the short term certificates.
* `keymaster` is the agent used to obtain the short-term certificates from the server (`keymasterd`)
* `keymaster-eventmon` is a daemon used to monitor a cluster of Keymaster clients. It uses [GRPC](https://grpc.io/) to collects authentication and certificate issuing activity to a single log file that can be retrieved from a single place (combining Keymaster logs with system logs (syslog) to verify all certificates uses (for at least SSH) can be attributed back to a specific Keymaster session is on the roadmap.
* `keymaster-unlocker` is use to ‘unseal’ the Keymaster when initialized with an encrypted CA. *keymaster-unlocker* requires a client side certificate that is signed by the adminCA.

From the user's perspective a single command is needed with no flags (after the first run). After running the client command successfully users get a 16h (or less) SSH and TLS certificates. On systems with a running [ssh-agent](https://en.wikipedia.org/wiki/Ssh-agent) the command also injects the certificate (with matching expiration time) so that no other interaction is needed to start using it with SSH.

For the service operators it requires adding the Keymaster certificates to the set of trusted certificates.

In general, the relationship between components is shown here:

![keymaster-keymasterd interaction image](docs/keymaster-overview.png)

Please see the
[design document](docs/Keymaster-DesignDoc.md) for more information.

## Getting Started
Pre-build binaries (both RPM and DEB) can be found here: [releases page](https://github.com/Cloud-Foundations/keymaster/releases) or you can build it from source (please see instructions below). The RPM and DEB packages contain both server and client binaries. The tarballs only contain the client binaries.

### Building from Source

#### Prerequisites
* go >= 1.8
* make
* gcc

For Windows (both gcc and gnu-make) use: [TDM-GCC (64 bit)](https://sourceforge.net/projects/tdm-gcc/).

#### Building
1. make get-deps
2. make

The make process will build the four binaries (keymasterd, keymaster, keymaster-unlocker, and keymaster-eventmond) described above.

### Running
Once you've installed (or compiled) the binaries follow the following instructions to setup a Keymaster environment

#### keymasterd (server)
The `keymasterd` service runs the following services:
* **Service Web Interface (default port 443)**: Access to the web interface running on port 443 (default) can be granted via LDAP or apache username/password files. For password backend Keymaster supports LDAP backends and apache password files.
* **Admin Management Interface (default port 6920)**: The service exposed on port 6920 allows administrators or log collection systems to collect logs generated by the `keymasterd` service.

To run `keymasterd` you will need to generate a config file. `keymasterd` facilitates this through the command-line arguments `-generateConfig` and `-alsoLogToStderr`. Running the `keymasterd` binary with these arguments will generate the following:
* A configuration file. By default `keymasterd` will write this file to `/etc/keymaster/config.yml`.
* The Keymaster CA key pair. The encrypted private key  (`masterkey.asc`) is an armored PGP file. For development (or if your trust model permits it) you can decrypt the private-key and write it to the filesystem. To decrypt the key run `gpg -ad $Filename`. Once decrypted set the `ssh_ca_filename` field in the `keymasterd` config file to the path of the decrypted master key.
* Server keys (for Testing Purposes only): the `server.pem` and `server.key` (self-signed for localhost)
* Admin CA certificate and key: The admin CA certificate (`adminCA.pem`) and key (`adminCA.key`) are used to generate certificates that grant access to the control port of the `keymasterd` management interface (default port 443).

Notice: Keymaster has a bug where the directory locations are not written correctly to the config file. Depending on the platform you're running Keymaster on the following workaround will apply:
* RPM (CentOS): Modify the following configuration items in your `config.yml` file:
    * `data_directory: /var/lib/keymaster `
    * `shared_data_directory: /usr/share/keymasterd/`.
* DEB (Debian/Ubuntu): Modify the following configuration items in your `config.yml` file:
    * `data_directory: /var/lib/keymaster `
    * `shared_data_directory: /usr/share/keymasterd/`.

##### Supported backend authentication methods
Several authentication methods are supported by the `keymasterd` service. You can separately specify which authentication methods you accept for the web backend (`allowed_auth_backends_for_webui`) and for obtaining certificates (`allowed_auth_backends_for_certs`).
* **LDAP**: For LDAP the `bind_pattern` is a printf string where `%s` is the place where the username will be substituted. For example for an 389ds/openldap string might be: `"uid=%s,ou=People,dc=example,dc=com`. To leverage LDAP authentication set the appropriate `allowed_auth_*` setting to `["ldap"]`.
* **Apache htpass**: The `passfile.htpass` file contains the usernames and their passwords allowed to access the `keymasterd` web interface. New users can be added via the following command: `htpasswd -B /etc/keymaster/passfile.htpass <username>`. `htpasswd` is distributed via the `httpd-tools` package. Keymaster will only accept htpass files that store BCRYPT encrypted credentials. To use Apache password files to authenticate users to the web interface set the following configuration item: `allowed_auth_*` to `["password"]`
* **U2F tokens**: To enable U2F tokens set set the appropriate `allowed_auth_*` setting to `["U2F"]``
* **VIP Manager**: To enable VIP Manager set set the appropriate `allowed_auth_*` setting to `["SymantecVIP"]`

##### Credential and Token Storage
Keymaster supports SQLite and PostgreSQL to store u2f tokens or username and passwords. The `storage_url` field in `config.yml` contains the connection information for the database. If no `storage_url` is defined Keymaster will use an SQLite database located in the configured data directory for Keymaster. An example of a PostgreSQL url is: `postgresql://dbusername:dbpassword.example.com/keymasterdbname`

##### Openid Connect IDP
To use keymasterd as an openid connect IDP please consult the documents [here](docs/keymaster-openidc-idp.md)

#### keymaster-unlocker
The `keymaster-unlocker` binary allows you to 'unseal' the Keymaster environment. This binary requires a client side certificate signed by the adminCA.

#### keymaster (client)
The first time you run the client it requires you to specify the Keymaster server with the option `-configHost`. The client will connect, retrieve and store the configuration from the server. Keymaster will always use TLS. For testing you can use the `-rootCAFilename` option to specify a (e.g self signed) certificate for testing. *The Keymaster clients will use the running OS CA store by default.*

Your certificate will be created in the home directory of the user that is running the `keymaster` command.

Note: Your username on your target (SSH) host and the username used to authenticate to the Keymaster server should be the same.

## Contributions

All contributions must be unencumbered. It is the responsibility of
the contributor to ensure compliance with all laws, copyrights,
patents and contracts.

## LICENSE
Copyright 2016-2019 Symantec Corporation.

Copyright 2019-2020 Cloud-Foundations.org

Licensed under the Apache License, Version 2.0 (the “License”); you
may not use this file except in compliance with the License.

You may obtain a copy of the License at
http://www.apache.org/licenses/LICENSE-2.0 Unless required by
applicable law or agreed to in writing, software distributed under the
License is distributed on an “AS IS” BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for
the specific language governing permissions and limitations under the
License.
