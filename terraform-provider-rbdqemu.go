/*
   This terraform provider manages Ceph RBD images and qemu VMs. In order
   to do that, this provider needs :
     ceph_rbduser - the ceph user when accessing an RBD resource
     ceph_hosts - a list of ceph admin hosts ceph_hosts
     qemu_hosts - a list of qemu hypevisor hosts
     ssh_private_key - path to key for logging into ceph and hypervisor hosts

   For each rbd image resource, we need to know
     osd_pool - the pool the rbd image will live in
     img_name - name of the rbd image
     img_size - size of the rbd image

   For each qemu VM resource, we need to know
     name - the name of the VM
     cpus - number of vCPUs
     mem_mb - amount of memory
     vlan - the VLAN the NIC is attached to
     mac - the mac address of the NIC
     vnc - the display instance, eg (":10")
     img_name - the RBD image that will be the OS disk

   Thus, here is a reference terraform configuration that uses this provider
   to provision an RBD image and then instantiate a qemu VM.

     provider "rbdqemu" {
       ceph_rbduser = "admin"
       ceph_hosts = [ "192.168.10.20", "192.168.10.18", "192.168.10.15" ]
       qemu_hosts = [ "192.168.3.100", "192.168.3.101", "192.168.3.102" ]
       ssh_private_key = "/root/.ssh/id_ed25519"
     }

     resource "rbdqemu_image" "helloImg" {
       osd_pool = "rbd"
       img_name = "helloImg"
       img_size = "6M"
     }

     resource "rbdqemu_vm" "helloVm" {
       name = "helloVm"
       cpus = 1
       mem_mb = 2048
       vlan = 10
       mac = "de:ad:be:ef:ca:fe"
       vnc = ":10"
       osd_pool = "rbd"
       img_name = "helloImg"
       depends_on = [rbd_image.helloImg]
     }

   Recall that terraform expects us to conform to a naming format :

     terraform-<type>-<name>

   Thus, our type is "provider" and our name is cfg_providerName. The resources
   we provide must therefore begin with cfg_providerName.
*/

package main ;

/* standard terraform imports */

import (
  "github.com/hashicorp/terraform-plugin-sdk/plugin"
  "github.com/hashicorp/terraform-plugin-sdk/terraform"
  "github.com/hashicorp/terraform-plugin-sdk/helper/schema"
) ;

/* my personal imports */

import (
  "os"
  "os/exec"
  "fmt"
  "time"
  "bufio"
  "regexp"
  "errors"
  "strconv"
  "strings"
  "runtime"
) ;

const cfg_providerName string = "rbdqemu" ;
const cfg_rbdResourceName string = cfg_providerName + "_image" ;
const cfg_vmResourceName string = cfg_providerName + "_vm" ;
const cfg_vmNamePrefix = "tf" ;
const cfg_logFile string = "provider.log" ;
const cfg_logMaxSize int64 = 131072 ;

const cfg_qemu_sys = "/usr/local/packages/qemu-4.1.0/bin/qemu-system-x86_64" ;
const cfg_qemu_img = "/usr/local/packages/qemu-4.1.0/bin/qemu-img" ;

var G_ssh_private_key string ;
var G_ceph_rbduser string ;
var G_ceph_hosts []string ;
var G_qemu_hosts []string ;

/* ------------------------------------------------------------------------- */

/*
   This function writes "msg" to our log file, prefixing it with a timestamp
   and adding a newline at the end.
*/

func f_log (msg string) {

  /* if the file is too big, rotate it now */

  sbuf, err := os.Stat (cfg_logFile) ;
  if (err == nil) && (sbuf.Size() > cfg_logMaxSize) {
    err = os.Rename (cfg_logFile, cfg_logFile + ".old") ;
    if (err != nil) {
      fmt.Printf ("WARNING: Cannot rotate %s - %s.\n", cfg_logFile, err) ;
    }
  }

  /* open/create file for appending */

  fd, err := os.OpenFile (cfg_logFile,
                          os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644) ;
  if (err != nil) {
    fmt.Printf ("WARNING: Cannot open %s - %s\n", cfg_logFile, err) ;
    return ;
  }
  defer fd.Close () ;

  /* figure out timestamp and calling function */

  now := time.Now().Format(time.RFC3339) ;
  pc, _, line, _ := runtime.Caller (1) ;
  fn := runtime.FuncForPC (pc) ;

  s := fmt.Sprintf ("%s %s:%d %s\n", now, fn.Name(), line, msg) ;
  _, err = fd.WriteString (s) ;
  if (err != nil) {
    fmt.Printf ("WARNING: Cannot write to %s - %s\n", cfg_logFile, err) ;
  }
}

/*
   This function is supplied a remote command to be executed by ssh'ing to
   a remote host. It returns stdout, stderr and fault status on completion.
*/

func f_ssh (host, rcmd string) (string, string, error) {

  ssh_args := [] string { "-i", G_ssh_private_key,
                          "-o", "StrictHostKeyChecking=no",
                          "-o", "BatchMode=yes",
                          "root@" + host, rcmd } ;
  ssh := exec.Command ("ssh", ssh_args...) ;
  stdout, _ := ssh.StdoutPipe () ;
  stderr, _ := ssh.StderrPipe () ;
  r_out := bufio.NewReader (stdout) ;
  r_err := bufio.NewReader (stderr) ;
  f_log (fmt.Sprintf ("connecting to %s.", host)) ;
  err := ssh.Start () ;
  if (err != nil) {
    f_log (fmt.Sprintf ("FATAL! Cannot exec ssh - %s", err)) ;
    return "", "", nil ;
  }

  /* read whatever comes out from the command and log it */

  var out_buf, err_buf string ;

  for {
    line, _, err := r_out.ReadLine () ;
    if (err != nil) {
      break ;
    }
    f_log ("stdout:" + string(line)) ;
    if (len(out_buf) == 0) {
      out_buf = string(line) ;
    } else {
      out_buf = out_buf + "\n" + string(line) ;
    }
  }
  for {
    line, _, err := r_err.ReadLine () ;
    if (err != nil) {
      break ;
    }
    f_log ("stderr:" + string(line)) ;
    if (len(err_buf) == 0) {
      err_buf = string(line) ;
    } else {
      err_buf = err_buf + "\n" + string(line) ;
    }
  }

  return out_buf, err_buf, ssh.Wait() ;
}

/* ------------------------------------------------------------------------- */

/*
   This function is called from either rsRbdExists() or rsRbdRead(). It returns
   the result (true/false if osd_pool/img_name exists on our ceph cluster and
   the error if something goes wrong.
*/

func f_rbdExists (osd_pool, img_name string) (bool, error) {

  /* setup the "rbd ls" command */

  rbd_cmd := fmt.Sprintf ("rbd ls -p %s", osd_pool) ;
  f_log (fmt.Sprintf ("{%s}", rbd_cmd)) ;
  out_buf, err_buf, fault := f_ssh (G_ceph_hosts[0], rbd_cmd) ;
  if (fault != nil) {
    return false, errors.New(fmt.Sprintf("ssh fault - %s", fault)) ;
  }
  if (len(err_buf) > 0) {
    return false, errors.New(fmt.Sprintf("rbd fault - %s", err_buf)) ;
  }

  /* attempt to locate "img_name" */

  for _, val := range (strings.Split(out_buf, "\n")) {
    if (strings.Compare (val, img_name) == 0) {
      f_log (fmt.Sprintf ("found '%s'", img_name)) ;
      return true, nil ;
    }
  }
  return false, nil ;
}

/*
   This function performs an ssh to each hypervisor, returning the host with
   the most amount of free memory (which must be more than "mem_mb").
*/

func f_getHypervisor (mem_mb int) string {

  var max_avail int ;
  var best_host string ;
  max_avail = 0 ;
  best_host = "" ;

  for _, v := range (G_qemu_hosts) {

    /*
       each host should return something like :
         MemAvailable:     131744 kB
    */

    out_buf, err_buf, fault := f_ssh (v, "grep MemAvailable /proc/meminfo") ;
    if ((fault != nil) || (len(err_buf) > 0)) {
      f_log (fmt.Sprintf ("ignoring %s.", v)) ;
    } else {
      tokens  := regexp.MustCompile("[ \t]+").Split(out_buf, -1) ;
      avail_kb, _ := strconv.Atoi (tokens[1]) ;
      if (avail_kb > max_avail) {
        max_avail = avail_kb ;
        best_host = v ;
      }
    }
  }

  f_log (fmt.Sprintf ("max_avail:%dkb best_host:%s", max_avail, best_host)) ;
  if (max_avail / 1024 > mem_mb) {
    return best_host ;
  }
  f_log (fmt.Sprintf ("WARNING: No hypervisor with %dMB free.", mem_mb)) ;
  return "" ;
}

/* ------------------------------------------------------------------------- */

func rsRbdCreate (d *schema.ResourceData, m interface{}) error {

  osd_pool := d.Get("osd_pool").(string) ;
  img_name := d.Get("img_name").(string) ;
  img_size := d.Get("img_size").(string) ;
  f_log (fmt.Sprintf ("%s/%s:%s", osd_pool, img_name, img_size)) ;

  /* setup the "rbd create ..." command */

  rbd_cmd := fmt.Sprintf ("rbd create --pool %s --image %s --size %s && " +
                          "rbd feature disable %s/%s " + 
                          "object-map fast-diff deep-flatten",
                          osd_pool, img_name, img_size,
                          osd_pool, img_name) ;
  f_log (fmt.Sprintf ("{%s}", rbd_cmd)) ;
  _, err_buf, fault := f_ssh (G_ceph_hosts[0], rbd_cmd) ;
  if (fault != nil) {
    f_log (fmt.Sprintf ("WARNING: %s", fault)) ;
    return fault ;
  } else if (len(err_buf) > 0) {
    return errors.New (err_buf) ;
  } 

  /*
     Even if the next step (ie, "qemu-img create") fails, we can't ignore the
     fact that the resource has already been created. Indicate success now.
  */

  f_log ("returning ID: " + osd_pool + "/" + img_name) ;
  d.SetId (osd_pool + "/" + img_name) ;	/* this indicates success */

  /* pick a hypervisor host and run "qemu-img create ..." */

  h := f_getHypervisor (1) ;
  if (len(h) < 1) {
    return nil ;
  }

  qemu_img_cmd := fmt.Sprintf ("%s create -f rbd rbd:%s/%s:id=%s %s",
                               cfg_qemu_img, osd_pool, img_name,
                               G_ceph_rbduser, img_size) ;
  f_log (fmt.Sprintf ("{%s}", qemu_img_cmd)) ;
  _, err_buf, fault = f_ssh (h, qemu_img_cmd) ;
  if (fault != nil) {
    f_log (fmt.Sprintf ("WARNING: %", fault)) ;
    return fault ;
  }
  if (len(err_buf) > 0) {
    return errors.New(err_buf)
  }
  return nil ;
}

func rsRbdRead (d *schema.ResourceData, m interface{}) error {

  osd_pool := d.Get("osd_pool").(string) ;
  img_name := d.Get("img_name").(string) ;
  f_log (fmt.Sprintf ("%s/%s", osd_pool, img_name)) ;

  result, fault := f_rbdExists(osd_pool, img_name) ;
  if (fault == nil) && (result == true) {
    d.SetId (osd_pool + "/" + img_name) ;	/* this indicates success */
  }
  return nil ;
}

func rsRbdUpdate (d *schema.ResourceData, m interface{}) error {
  f_log ("") ;
  return errors.New ("feature not implemented") ;
}

func rsRbdDelete (d *schema.ResourceData, m interface{}) error {

  osd_pool := d.Get("osd_pool").(string) ;
  img_name := d.Get("img_name").(string) ;
  f_log (fmt.Sprintf("%s/%s", osd_pool, img_name)) ;

  /* setup the "rbd rm" command */

  rbd_cmd := fmt.Sprintf ("rbd rm --no-progress %s/%s", osd_pool, img_name) ;
  f_log (fmt.Sprintf ("{%s}", rbd_cmd)) ;
  _, err_buf, fault := f_ssh (G_ceph_hosts[0], rbd_cmd) ;
  if (fault != nil) {
    f_log (fmt.Sprintf ("WARNING: %s", fault)) ;
    return fault ;
  }
  if (len(err_buf) > 0) {
    return errors.New(err_buf) ;
  }
  return nil ;
}

func rsRbdExists (d *schema.ResourceData, m interface{}) (bool, error) {

  osd_pool := d.Get("osd_pool").(string) ;
  img_name := d.Get("img_name").(string) ;
  f_log (fmt.Sprintf ("%s/%s", osd_pool, img_name)) ;

  return f_rbdExists(osd_pool, img_name) ;
}

/* ------------------------------------------------------------------------- */

/*
   This function is called from rsVmExists(), rsVmRead() or rsVmDelete(). It
   returns the hypervisor host running the VM, its pid and sets "error" if
   something goes wrong.
*/

func f_vmExists (name string) (string, int, error) {

  id := fmt.Sprintf ("%s-%s", cfg_vmNamePrefix, name) ;
  f_log ("searching for : " + id) ;

  ssh_cmd := fmt.Sprintf ("ps axwww -o 'pid args' | grep -v grep | " +
                          "grep -w '%s' ; /bin/true", id) ;
  for _, v := range (G_qemu_hosts) {

    /*
       grab the stdout from the ssh_cmd, a match ought to look like :

        2533650 /usr/local/packages/qemu-4.1.0/bin/qemu-system-x86_64
        -name tf-helloVm -smp 1 -m 128 -vnc :20 ...

       We expect the PID, "-name" and "id" to be in very specific positions,
       otherwise return as a negative result.
    */

    stdout, stderr, fault := f_ssh (v, ssh_cmd) ;
    if (fault != nil) {
      f_log (fmt.Sprintf ("unable to search %s - %s", v, fault)) ;
      return "", 0, fault ;
    }
    if (len(stderr) > 0) {
      f_log (fmt.Sprintf ("error on %s - %s", v, stderr)) ;
      return "", 0, errors.New(stderr) ;
    }
    tokens := strings.Fields (stdout) ;
    if (len(tokens) > 3) {
      pid, _ := strconv.Atoi (tokens[0]) ;
      if ((strings.Compare (tokens[2], "-name") == 0) &&
          (strings.Compare (tokens[3], id) == 0)) {
        f_log (fmt.Sprintf ("found '%s' on '%s' pid:%d", id, v, pid))
        return v, pid, nil ;
      } else {
        f_log (fmt.Sprintf ("unexpected process [%s]", stdout))
      }
    } // ... iterate over one line of "ps" output
  } // ... iterate over all hypervisor hosts

  f_log (fmt.Sprintf ("vm %s not found", id)) ;
  return "", 0, nil ;
}

/*
   This function fires up a VM on the designated hypervisor host. Note that
   the VM's name is "id", which is "name" prefixed with cfg_vmNamePrefix. This
   id is returned to terraform to indicate success. If something goes wrong,
   we don't set "id". This function always returns nil.
*/

func rsVmCreate (d *schema.ResourceData, m interface{}) error {
  vnc := d.Get("vnc").(string) ;
  mac := d.Get("mac").(string) ;
  cpus := d.Get("cpus").(int) ;
  vlan := d.Get("vlan").(int) ;
  name := d.Get("name").(string) ;
  mem_mb := d.Get("mem_mb").(int) ;
  osd_pool := d.Get("osd_pool").(string) ;
  img_name := d.Get("img_name").(string) ;
  f_log (fmt.Sprintf ("name:%s cpus:%d mem_mb:%d vlan:%d vnc:%s img_name:%s",
                      name, cpus, mem_mb, vlan, vnc, img_name)) ;

  h := f_getHypervisor (mem_mb) ;
  if (len(h) < 1) {
    return nil ;
  }
  id := cfg_vmNamePrefix + "-" + name ;

  qemu_cmd := fmt.Sprintf ("%s " +
                           "-name %s " +
                           "-smp %d " +
                           "-m %d " +
                           "-vnc %s " +
                           "-drive format=rbd,file=rbd:%s/%s," +
                             "cache=writeback " +
                           "-nic tap,script=/root/bin/add_tap%d.sh," +
                             "model=virtio-net-pci,mac=%s " +
                           "-vga vmware " +
                           "-enable-kvm " +
                           "-usb " +
                           "-device usb-tablet " +
                           "-daemonize", 
                           cfg_qemu_sys,
                           id,
                           cpus,
                           mem_mb,
                           vnc,
                           osd_pool, img_name,
                           vlan, mac) ;
  _, err_buf, fault := f_ssh (h, qemu_cmd)
  if (fault != nil) {
    f_log (fmt.Sprintf ("WARNING: %s", fault)) ;
    return fault ;
  }
  if (len(err_buf) > 0) {
    return errors.New(err_buf) ;
  }

  f_log ("returning ID: " + id) ;
  d.SetId (id) ; /* this indicates success */
  return nil ;
}

func rsVmRead (d *schema.ResourceData, m interface{}) error {
  name := d.Get("name").(string) ;
  f_log ("searching for : " + name) ;
  hypervisor, _, fault := f_vmExists (name) ;
  if (fault != nil) && (len(hypervisor) > 0) {
    d.SetId (cfg_vmNamePrefix + "-" + name) ;   /* this indicates success */
  }
  return nil ;
}

func rsVmUpdate (d *schema.ResourceData, m interface{}) error {
  f_log ("") ;
  return errors.New ("feature not implemented") ;
}

/*
   This function deletes a running VM (ie, kill the qemu process). It returns
   nil on success, otherwise an error is returned.
*/

func rsVmDelete (d *schema.ResourceData, m interface{}) error {
  name := d.Get("name").(string) ;
  f_log ("deleting VM : " + name) ;
  hypervisor, pid, fault := f_vmExists (name) ;
  if (len(hypervisor) > 0) && (pid > 1) && (fault == nil) {
    ssh_cmd := fmt.Sprintf ("kill %d", pid) ;
    _, _, fault := f_ssh (hypervisor, ssh_cmd) ;
    if (fault != nil) {
      return errors.New (fmt.Sprintf ("Failed to delete %s pid:%d on %s - %s",
                                      name, pid, hypervisor)) ;
    } else {
      return nil ;
    }
  }
  return errors.New ("Could not locate VM and pid of " + name)
}

/*
   This function checks if the requested VM is running in any one of our
   hypervisor hosts. We look for a VM with "-name" matching what we're looking
   for. We return a true/false depending on our search, and set "error" if
   something went wrong.
*/

func rsVmExists (d *schema.ResourceData, m interface{}) (bool, error) {
  name := d.Get("name").(string) ;
  f_log ("searching for : " + name) ;
  hypervisor, _, fault := f_vmExists (name) ;
  if (fault != nil) {
    return false, nil ;
  }
  if (len(hypervisor) > 0) {
    return true, nil ;
  } else {
    return false, nil ;
  }
}

func rbdConfig (d *schema.ResourceData) (interface{}, error) {
  G_ssh_private_key = d.Get("ssh_private_key").(string) ;
  G_ceph_rbduser = d.Get("ceph_rbduser").(string) ;
  for _, v := range (d.Get("ceph_hosts").(*schema.Set).List()) {
    G_ceph_hosts = append (G_ceph_hosts, v.(string)) ;
  }
  for _, v := range (d.Get("qemu_hosts").(*schema.Set).List()) {
    G_qemu_hosts = append (G_qemu_hosts, v.(string)) ;
  }
  return nil, nil ;
}

/* ================================= */
/* Define rbd *RESOURCE* schema here */
/* ================================= */

func rbdItem () *schema.Resource {
  return &schema.Resource {
    Create: rsRbdCreate,
    Read:   rsRbdRead,
    Update: rsRbdUpdate,
    Delete: rsRbdDelete,
    Exists: rsRbdExists,

    Schema: map[string] *schema.Schema {
      "osd_pool": {
        Type: schema.TypeString,
        Required: true,
      },
      "img_name": {
        Type: schema.TypeString,
        Required: true,
      },
      "img_size": {
        Type: schema.TypeString,
        Required: true,
      },
    },
  }
}

func vmItem () *schema.Resource {
  return &schema.Resource {
    Create: rsVmCreate,
    Read:   rsVmRead,
    Update: rsVmUpdate,
    Delete: rsVmDelete,
    Exists: rsVmExists,

    Schema: map[string] *schema.Schema {
      "name": {
        Type: schema.TypeString,
        Required: true,
      },
      "cpus": {
        Type: schema.TypeInt,
        Required: true,
      },
      "mem_mb": {
        Type: schema.TypeInt,
        Required: true,
      },
      "vlan": {
        Type: schema.TypeInt,
        Required: true,
      },
      "mac": {
        Type: schema.TypeString,
        Required: true,
      },
      "vnc": {
        Type: schema.TypeString,
        Required: true,
      },
      "osd_pool": {
        Type: schema.TypeString,
        Required: true,
      },
      "img_name": {
        Type: schema.TypeString,
        Required: true,
      },
    },
  }
}

/* ============================= */
/* Define *PROVIDER* schema here */
/* ============================= */

func rbdProvider() terraform.ResourceProvider {
  return &schema.Provider {
    Schema: map[string] *schema.Schema {
      "ceph_hosts": {
        Type: schema.TypeSet,
        Elem: &schema.Schema {
          Type: schema.TypeString,
        },
        Required: true,
      },
      "qemu_hosts": {
        Type: schema.TypeSet,
        Elem: &schema.Schema {
          Type: schema.TypeString,
        },
        Required: true,
      },
      "ssh_private_key": {
        Type: schema.TypeString,
        Required: true,
      },
      "ceph_rbduser": {
        Type: schema.TypeString,
        Required: true,
      },
    },
    ResourcesMap: map[string] *schema.Resource {
      cfg_rbdResourceName: rbdItem (),
      cfg_vmResourceName: vmItem (),
    },
    ConfigureFunc: rbdConfig,
  }
}

func main() {
  f_log (os.Args[0]) ;

  /* ok, now let's behave like a terraform provider */

  plugin.Serve (&plugin.ServeOpts {
    ProviderFunc: func() terraform.ResourceProvider {
      return rbdProvider() ;
    },
  })
}

