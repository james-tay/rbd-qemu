provider "rbdqemu" {
  ceph_rbduser = "admin"
  ceph_hosts = [ "192.168.3.8", "192.168.3.9", "192.168.3.12" ]
  qemu_hosts = [ "192.168.3.8", "192.168.3.9", "192.168.3.12" ]
  ssh_private_key = "/home/jamsie/work/terraform/terraform_key"
}

resource "rbdqemu_boot" "osDisk" {
  osd_pool = "rbd"
  snap_name = "tmpl-debian10-os@initial"
  img_name = "helloOS"
}

resource "rbdqemu_disk" "dataDisk1" {
  osd_pool = "rbd"
  img_name = "helloData1"
  img_size = "6M"
}

resource "rbdqemu_disk" "dataDisk2" {
  osd_pool = "rbd"
  img_name = "helloData2"
  img_size = "6M"
}

resource "rbdqemu_vm" "helloVm" {
  name = "helloVm"
  cpus = 1
  mem_mb = 384
  vlan = 10
  vnc = ":20"
  mac = "de:ad:be:ef:ca:fe"
  osd_pool = "rbd"
  boot_disk = "helloOS"
  extra_disks = [
    "helloData1",
    "helloData2"
  ]
  depends_on = [
    rbdqemu_boot.osDisk,
    rbdqemu_disk.dataDisk1,
    rbdqemu_disk.dataDisk2
  ]
}

