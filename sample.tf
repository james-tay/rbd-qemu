provider "rbdqemu" {
  ceph_rbduser = "admin"
  ceph_hosts = [ "192.168.3.8", "192.168.3.9", "192.168.3.12" ]
  qemu_hosts = [ "192.168.3.8", "192.168.3.9", "192.168.3.12" ]
  ssh_private_key = "/home/jamsie/work/terraform/terraform_key"
}

resource "rbdqemu_image" "helloImg" {
  osd_pool = "rbd"
  img_name = "helloImg"
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
  img_name = "helloImg"
  depends_on = [rbdqemu_image.helloImg]
}
