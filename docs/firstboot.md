# First-boot configuration

How an installed system gets its hostname, users, keys and (containers)
network: `firstBoot:`
on a node in `site.yaml` or in an image config (the node's wins), one of
three, exclusively. With ignition or cloud-init, stage0 adds no hostname or
keys of its own, so two tools never write the same files.

> Last verified: 2026-10-28

| `firstBoot` | Who configures | Needs | Verified |
| --- | --- | --- | --- |
| `stage0` (default) | stage0. Disk images: `systemd.hostname=` and root's keys as a systemd credential on the installed `cmdline.txt`; cloud-init images' `user-data` gets the hostname. Containers: the agent writes the network, hostname and root's keys into the root ([below](#stage0-on-the-container-route)) | containers: the image's network tool named | every install so far; containers' network on k52, 2026-10-28 |
| `ignition` | the installer's Ignition applies the config's `passwd` (users, keys), `storage.files` and `systemd` units | a container with Ignition in its installer and an SELinux policy | k52, 2026-10-28 |
| `cloud-init` | cloud-init on first boot, from the user's `user-data` (and `network-config`) and stage0's `meta-data` | cloud-init in the image | k400 (container), k52 (Ubuntu disk image), vm1 (Rocky cloud image), 2026-10-28 |

## stage0 on the container route

The agent writes the installed system's own configuration, as the OS
would keep it, after unpacking and before SELinux labelling:

- **Network**, for the image's tool: `network: networkd` or `network:
  networkmanager` in the image config, else the label
  `pi.provision.network` (a JSON string); stage0 refuses the install
  without one. Interfaces come from the node's `interfaces:` in
  `site.yaml`; none means DHCP on the NIC it boots on:

  ```yaml
  interfaces:
    - dhcp: true                 # no mac: the NIC it boots on
      vlans: [{id: 23, dhcp: true}]
    - mac: d8:3a:dd:00:00:01     # another NIC, static
      addresses: [10.0.4.5/24]
      gateway: 10.0.4.1
      dns: [10.0.4.1]
  ```

  NICs are matched by MAC (the boot NIC's is the one that reaches stage0),
  so kernel names (`eth0`, `end0`) don't matter. `dhcp` means DHCPv4 and
  IPv6 autoconfiguration; without it, only the addresses given. A tagged
  VLAN is the interface `vlan<id>`. networkd gets
  `/etc/systemd/network/10-stage0-*` (before the image's own files);
  NetworkManager gets keyfiles `stage0-*.nmconnection` (mode 600, fixed
  UUIDs per node).
- **Hostname:** `/etc/hostname`. **Root's keys:** `/root/.ssh/authorized_keys`
  (`sshKeys:` in `site.yaml`). Nothing goes on the kernel command line.
- **Identity:** an image's `/etc/machine-id` is emptied, so systemd makes a
  new one on first boot; `containers/rocky10-rpi` shipped one, so every
  node had the same machine ID (and DHCP identity). An image's SSH host
  keys are replaced by its own `ssh-keygen -A` (chrooted); not yet seen,
  as the Rocky container ships none.
- The result's `configured` lists what was written.

Verified (k52, Rocky container, NetworkManager): hostname `k52`; a new
machine ID; `stage0-eth0` on `end0` with its lease and SLAAC address;
`vlan23` with an address and the default route from VLAN 23's DHCP.

## ignition

- The config is the node's or image config's `ignition:` (or the
  container's `pi.provision.ignition` label). With `storage.disks` it also
  partitions ([containers](containers.md#partitioning-by-ignition));
  without, it configures first boot only and the layout comes from
  elsewhere. A partitioning config and a separate first-boot one are
  refused: Ignition takes one config.
- After unpacking, with every partition mounted in the tree, the agent runs
  `ignition --root=/mnt/root --platform=metal --stage=files`, which uses
  the config Ignition fetched at boot. Users are made with `useradd
  --root`.
- **Keys:** Ignition writes them to `~/.ssh/authorized_keys.d/ignition`;
  Rocky's sshd reads only `~/.ssh/authorized_keys`, and takes no wildcard
  in `AuthorizedKeysFile` (`authorized_keys.d/*` was tried literally). The
  container has an sshd drop-in naming that file.
- **SELinux:** Rocky's Ignition always relabels what it writes, with the
  policy `/etc/selinux/config` names (it ignores `SELINUX=disabled`); with
  no policy it fails ("failed to open /etc/selinux/config", then setfiles
  "file_contexts: No such file"). So the image needs a policy; the agent
  says so before running Ignition.

Verified (k52): user `admin` in `wheel` logged in with the config's key;
hostname and motd from its files; root refused.

## cloud-init

- `cloudInit: {userData: <file>, networkConfig: <file>}` beside
  `firstBoot` (paths relative to `site.yaml` or `images/`). stage0 writes
  `meta-data`: `instance-id: <node>-<time>` (a new instance each install)
  and `local-hostname`.
- **Containers:** the agent writes the seed to the installed root's
  `/var/lib/cloud/seed/nocloud/` (mode 600), which cloud-init always reads.
- **Disk images:** the seed goes on the boot partition, replacing the
  image's own `user-data` and `meta-data`; Ubuntu, Raspberry Pi OS and Alma
  read NoCloud from there.
- **VMs:** stage0 serves the files for the NoCloud pointer in the SMBIOS
  serial, the user's `user-data` in place of the one stage0 writes.

Verified (k400, Rocky container): user `cloud` in `wheel` logged in with
its key; hostname `k400` from stage0's meta-data; motd from `write_files`;
`cloud-init status: done`; root refused. And (k52, Ubuntu 26.04 disk
image, seed on the boot partition): the same, with no `ubuntu` user (the
image's user-data replaced). Ubuntu's own `network-config`, left in place
without the user's, asks DHCP on `eth0`, which a Pi 5 calls `end0`, so it
got no IPv4 address (as in stage0 mode); give a `networkConfig` to fix
that. And (vm1, Rocky 10.2 cloud image): the same, hostname `vm1`.

## SELinux labels on container installs

Not first-boot configuration, but found with it: layers built on a host
without SELinux carry no labels, and an enforcing system with unlabelled
files can't start systemd ("Failed to set SELinux security context …
Permission denied"); a first-boot `/.autorelabel` never gets to run. When
the installed root has a policy and SELinux on, the agent labels
everything with `setfiles`, naming each ext4 filesystem (a `/var` mounted
inside the root was left unlabelled otherwise, and NetworkManager failed).
It adds about a second.
