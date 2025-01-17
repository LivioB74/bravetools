package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"os/user"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"path/filepath"

	"github.com/bravetools/bravetools/db"
	"github.com/bravetools/bravetools/shared"
	"github.com/google/uuid"
	"github.com/olekukonko/tablewriter"
)

// Functions exposed to commands.go

// AddRemote sets connection to Brave platform
func (bh *BraveHost) AddRemote() error {
	err := AddRemote(bh.Remote, bh.Settings.Trust)
	if err != nil {
		return errors.New("failed to add remote host: " + err.Error())
	}

	return nil
}

// ImportLocalImage import tarball into local images folder
func (bh *BraveHost) ImportLocalImage(sourcePath string) error {
	home, _ := os.UserHomeDir()
	imageStore := path.Join(home, shared.ImageStore)

	_, imageName := filepath.Split(sourcePath)

	image, err := ImageFromFilename(imageName)
	if err != nil {
		return err
	}

	if _, err = matchLocalImagePath(image); err == nil {
		return fmt.Errorf("image %q already exists in local image store", image)
	}

	imagePath := filepath.Join(imageStore, image.ToBasename()+".tar.gz")
	hashFile := imagePath + ".md5"

	err = shared.CopyFile(sourcePath, imagePath)
	if err != nil {
		return errors.New("failed to copy image archive to local image store: " + err.Error())
	}

	imageHash, err := shared.FileHash(sourcePath)
	if err != nil {
		return errors.New("failed to generate image hash: " + err.Error())
	}

	// Write image hash to a file
	f, err := os.Create(hashFile)
	if err != nil {
		return errors.New(err.Error())
	}
	defer f.Close()

	_, err = f.WriteString(imageHash)
	if err != nil {
		return errors.New(err.Error())
	}

	fmt.Printf("Imported file %q into bravetools as image %q\n", imageName, image)

	return nil
}

// PrintLocalImages prints the images in image store
func (bh *BraveHost) PrintLocalImages() error {
	images, err := GetLocalImages()
	if err != nil {
		return err
	}

	if len(images) == 0 {
		fmt.Println("No local images")
		return nil
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Image", "Version", "Arch", "Created", "Size", "Hash"})

	for _, image := range images {
		created := int(time.Since(image.modTime).Hours() / 24)
		var timeUnit string
		if created > 1 {
			timeUnit = strconv.Itoa(created) + " days ago"
		} else if created == 1 {
			timeUnit = strconv.Itoa(created) + " day ago"
		} else {
			timeUnit = "just now"
		}

		r := []string{image.Name, image.Version, image.Architecture, timeUnit, shared.FormatByteCountSI(image.size), image.hashString}
		table.Append(r)
	}

	table.SetAutoWrapText(false)
	table.SetAutoFormatHeaders(true)
	table.SetHeaderAlignment(tablewriter.ALIGN_LEFT)
	table.SetAlignment(tablewriter.ALIGN_LEFT)
	table.SetCenterSeparator("")
	table.SetColumnSeparator("")
	table.SetRowSeparator("")
	table.SetHeaderLine(false)
	table.SetBorder(false)
	table.SetTablePadding("\t")
	table.SetNoWhiteSpace(true)
	table.Render()

	return nil
}

// DeleteLocalImage deletes a local image
func (bh *BraveHost) DeleteLocalImage(name string, legacy bool) error {
	var image BravetoolsImage
	var err error

	if legacy {
		image, err = ParseLegacyImageString(name)
		if err != nil {
			return err
		}
	} else {
		image, err = ParseImageString(name)
		if err != nil {
			return err
		}
	}

	imagePath, err := matchLocalImagePath(image)
	if err != nil {
		return err
	}
	imageHash := imagePath + ".md5"

	err = os.Remove(imagePath)
	if err != nil {
		return err
	}

	err = os.Remove(imageHash)
	if err != nil {
		return err
	}

	return nil
}

// HostInfo returns useful information about brave host
func (bh *BraveHost) HostInfo(short bool) error {
	info, err := bh.Backend.Info()
	if err != nil {
		return errors.New("failed to connect to host: " + err.Error())
	}

	if short {
		fmt.Println(info.IPv4)
		return nil
	}

	if info.State == "Stopped" {
		return errors.New("cannot connect to Bravetools remote, ensure it is up and running")
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Name", "State", "IPv4", "Disk", "Memory", "CPU"})

	r := []string{info.Name, info.State, info.IPv4,
		info.Disk.UsedStorage + " of " + info.Disk.TotalStorage,
		info.Memory.UsedStorage + " of " + info.Memory.TotalStorage, info.CPU}

	table.Append(r)
	table.SetAutoWrapText(false)
	table.SetAutoFormatHeaders(true)
	table.SetHeaderAlignment(tablewriter.ALIGN_LEFT)
	table.SetAlignment(tablewriter.ALIGN_LEFT)
	table.SetCenterSeparator("")
	table.SetColumnSeparator("")
	table.SetRowSeparator("")
	table.SetHeaderLine(false)
	table.SetBorder(false)
	table.SetTablePadding("\t")
	table.SetNoWhiteSpace(true)
	table.Render()

	return nil
}

// GetUnitNames returns a list of all unit names, including remote units. Errors encountered will result in skipping the faulty remote's units.
func (bh *BraveHost) GetUnitNames() []string {
	var unitNames []string

	// Load all units on all remotes

	remoteNames, err := ListRemotes()
	if err != nil {
		return unitNames
	}

	for i := range remoteNames {
		deployRemote, err := LoadRemoteSettings(remoteNames[i])
		if err != nil {
			continue
		}

		// If no auth, this isn't a deploy remote unless unix protocol
		if (deployRemote.key == "" || deployRemote.cert == "") && deployRemote.Protocol != "unix" {
			continue
		}

		lxdServer, err := GetLXDInstanceServer(deployRemote)
		if err != nil {
			log.Printf("failed to connect to %q remote, skipping", deployRemote.Name)
			continue
		}

		remoteUnits, err := GetUnits(lxdServer, deployRemote.Profile)
		if err != nil {
			continue
		}

		// Prefix unit name with remote name
		if deployRemote.Name != shared.BravetoolsRemote {
			for j := range remoteUnits {
				unitNames = append(unitNames, deployRemote.Name+":"+remoteUnits[j].Name)
			}
		} else {
			for j := range remoteUnits {
				unitNames = append(unitNames, remoteUnits[j].Name)
			}
		}
	}

	return unitNames
}

// PrintUnits prints all LXD containers on remote host
func (bh *BraveHost) PrintUnits(backend Backend, remoteName string) error {
	var units []shared.BraveUnit

	if remoteName != "" {
		deployRemote, err := LoadRemoteSettings(remoteName)
		if err != nil {
			return err
		}

		lxdServer, err := GetLXDInstanceServer(deployRemote)
		if err != nil {
			return err
		}

		deployProfile := deployRemote.Profile
		if deployProfile == "" {
			deployProfile = deployRemote.Profile
		}

		units, err = GetUnits(lxdServer, deployProfile)
		if err != nil {
			return errors.New("Failed to list units: " + err.Error())
		}
	} else {
		// Load all units on all remotes

		remoteNames, err := ListRemotes()
		if err != nil {
			return err
		}

		for i := range remoteNames {
			deployRemote, err := LoadRemoteSettings(remoteNames[i])
			if err != nil {
				return err
			}

			// If no auth, this isn't a deploy remote unless unix protocol
			if (deployRemote.key == "" || deployRemote.cert == "") && deployRemote.Protocol != "unix" {
				continue
			}

			lxdServer, err := GetLXDInstanceServer(deployRemote)
			if err != nil {
				log.Printf("failed to connect to %q remote, skipping", deployRemote.Name)
				continue
			}

			remoteUnits, err := GetUnits(lxdServer, deployRemote.Profile)
			if err != nil {
				return errors.New("Failed to list units: " + err.Error())
			}

			// Prefix unit name with remote name
			if deployRemote.Name != shared.BravetoolsRemote {
				for j := range remoteUnits {
					remoteUnits[j].Name = deployRemote.Name + ":" + remoteUnits[j].Name
				}
			}
			units = append(units, remoteUnits...)
		}
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Name", "Status", "IPv4", "Mounts", "Ports"})
	for _, u := range units {
		name := u.Name
		status := u.Status
		address := u.Address

		disk := ""
		for _, diskDevice := range u.Disk {
			// Filter storage pools from output
			if strings.HasPrefix(diskDevice.Name, "brave_") && diskDevice.Source != "" {
				// Format presentation - trim excessively long paths. Ensure slashes are present
				mountSourceStr := diskDevice.Source
				if len(diskDevice.Source) > 32 {
					mountSourceStr = mountSourceStr[:32] + "..."
				}

				mountTargetStr := diskDevice.Path
				if len(diskDevice.Path) > 32 {
					mountTargetStr = mountTargetStr[:32] + "..."
				}
				if !strings.HasPrefix(mountTargetStr, "/") {
					mountTargetStr = "/" + mountTargetStr
				}
				disk += mountSourceStr + "->" + mountTargetStr + "\n"
			}
		}

		proxy := ""
		for _, proxyDevice := range u.Proxy {
			if proxyDevice.Name != "" {
				connectIP := strings.Split(proxyDevice.ConnectIP, ":")[2]
				listenIP := strings.Split(proxyDevice.ListenIP, ":")[2]
				proxy += connectIP + ":" + listenIP + "\n"
			}
		}

		r := []string{name, status, address, disk, proxy}
		table.Append(r)
	}
	table.SetRowLine(false)
	table.SetAutoWrapText(false)
	table.SetAutoFormatHeaders(true)
	table.SetHeaderAlignment(tablewriter.ALIGN_LEFT)
	table.SetAlignment(tablewriter.ALIGN_LEFT)
	table.SetCenterSeparator("")
	table.SetColumnSeparator("")
	table.SetRowSeparator("")
	table.SetHeaderLine(false)
	table.SetBorder(false)
	table.SetTablePadding("\t")
	table.SetNoWhiteSpace(true)
	table.Render()

	return nil
}

// UmountShare ..
func (bh *BraveHost) UmountShare(unit string, target string) error {

	backend := bh.Settings.BackendSettings.Type

	lxdServer, err := GetLXDInstanceServer(bh.Remote)
	if err != nil {
		return err
	}

	// Device name is derived from unit and target path
	target = cleanMountTargetPath(target)
	deviceName := getDiskDeviceHash(unit, target)

	switch backend {
	case "multipass":

		path, err := DeleteDevice(lxdServer, unit, deviceName)
		if err != nil {
			return fmt.Errorf("failed to umount %q from unit %q: %s", target, unit, err.Error())
		}

		cmd := fmt.Sprintf(`if [ -d "%s" ]; then echo "exists"; else echo "none"; fi`, path)
		output, err := shared.ExecCommandWReturn("multipass",
			"exec",
			bh.Settings.Name,
			"--", "bash", "-c",
			cmd)
		if err != nil {
			return errors.New("could not check directory: " + err.Error())
		}
		output = strings.Trim(output, "\n")

		hostOs := runtime.GOOS
		if hostOs == "windows" {
			path = strings.Replace(path, string(filepath.Separator), "/", -1)
		}

		if output == "exists" {
			err = shared.ExecCommand("multipass",
				"umount",
				bh.Settings.Name+":"+path)
			if err != nil {
				return fmt.Errorf("failed to unmount %q from multipass host: %s", path, err)
			}

			err = shared.ExecCommand("multipass", "exec", bh.Settings.Name, "rmdir", path)
			if err != nil {
				log.Printf("failed to cleanup empty leftover mountpoint dir %q\n", path)
			}
		}

	case "lxd":
		_, err := DeleteDevice(lxdServer, unit, deviceName)
		if err != nil {
			return errors.New("failed to umount " + target + ": " + err.Error())
		}
	}

	volume, _ := GetVolume(lxdServer, bh.Settings.StoragePool.Name)
	if len(volume.UsedBy) == 0 {
		DeleteVolume(lxdServer, bh.Settings.StoragePool.Name, volume)

		return nil
	}

	return nil
}

// MountShare ..
func (bh *BraveHost) MountShare(source string, destUnit string, destPath string) error {

	lxdServer, err := GetLXDInstanceServer(bh.Remote)
	if err != nil {
		return err
	}

	names, err := GetUnits(lxdServer, bh.Remote.Profile)
	if err != nil {
		return errors.New("failed to access units")
	}

	var found = false
	for _, n := range names {
		if n.Name == destUnit {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("unit %q not found", destUnit)
	}

	backend := bh.Settings.BackendSettings.Type
	var sourceUnit string
	var sourcePath string

	sourceSlice := strings.SplitN(source, ":", -1)
	if len(sourceSlice) > 2 {
		return fmt.Errorf("failed to parse source %q. Accepted form [UNIT:]<path>", source)
	} else if len(sourceSlice) == 2 {
		sourceUnit = sourceSlice[0]
		sourcePath = filepath.ToSlash(sourceSlice[1])
	} else if len(sourceSlice) == 1 {
		sourceUnit = ""
		sourcePath, err = filepath.Abs(source)
		if err != nil {
			return err
		}
	}

	destPath = cleanMountTargetPath(destPath)

	// Unit-to-unit volume creation and mounting is same across backends
	if sourceUnit != "" {
		err := createSharedVolume(lxdServer,
			bh.Settings.StoragePool.Name,
			sourceUnit,
			sourcePath,
			destUnit,
			destPath)
		if err != nil {
			// Or error, unmount and cleanup newly created volume
			if err := bh.UmountShare(sourceUnit, sourcePath); err != nil {
				log.Println(err)
			}
			if err := bh.UmountShare(destUnit, destPath); err != nil {
				log.Println(err)
			}
		}
		return err
	}

	switch backend {
	case "multipass":
		sharedDirectory := path.Join("/home/ubuntu", "volumes", getDiskDeviceHash(destUnit, destPath))

		err := shared.ExecCommand("multipass",
			"mount",
			sourcePath,
			bh.Settings.Name+":"+sharedDirectory)
		if err != nil {
			return errors.New("Failed to initialize mount on host: " + err.Error())
		}

		err = MountDirectory(lxdServer, sharedDirectory, destUnit, destPath)
		if err != nil {
			if err := shared.ExecCommand("multipass", "umount", bh.Settings.Name+":"+sharedDirectory); err != nil {
				log.Printf("failed to cleanup multipass mount %q\n", sharedDirectory)
			}
			return errors.New("failed to mount " + sourcePath + " to " + destUnit + ":" + destPath + " : " + err.Error())
		}
	case "lxd":
		err := MountDirectory(lxdServer, sourcePath, destUnit, destPath)
		if err != nil {
			return errors.New("failed to mount " + source + " to " + destUnit + ":" + destPath + " : " + err.Error())
		}
	}

	return nil
}

func (bh *BraveHost) ListAllMounts() error {
	lxdServer, err := GetLXDInstanceServer(bh.Remote)
	if err != nil {
		return err
	}

	units, err := GetUnits(lxdServer, bh.Settings.Profile)
	if err != nil {
		return fmt.Errorf("failed to retrieve units: %s", err)
	}

	for _, unit := range units {
		fmt.Printf("Mounts for %s:\n", unit.Name)
		err := bh.ListMounts(unit.Name)
		if err != nil {
			return fmt.Errorf("failed to retrieve mounts for unit %q: %s", unit.Name, err)
		}
	}

	return nil
}

func (bh *BraveHost) ListMounts(unitName string) error {
	lxdServer, err := GetLXDInstanceServer(bh.Remote)
	if err != nil {
		return err
	}

	unit, _, err := lxdServer.GetInstance(unitName)
	if err != nil {
		return fmt.Errorf("could not get unit %q", unitName)
	}

	var devices []map[string]string

	// Pull bravetools-managed devices from map into slice
	for deviceName, device := range unit.Devices {
		if strings.HasPrefix(deviceName, "brave_") {
			_, hasType := device["type"]
			_, hasSource := device["source"]

			if hasType && hasSource && device["type"] == "disk" {
				devices = append(devices, device)
			}
		}
	}

	// Sort the slice of devices by: 1) source length and 2) by alphabetical order
	// Sorting the devices like this makes output deterministic and predictable
	sort.Slice(devices, func(i int, j int) bool {
		l1, l2 := len(devices[i]["source"]), len(devices[j]["source"])
		if l1 != l2 {
			return l1 < l2
		}
		return devices[i]["source"] < devices[j]["source"]
	})

	for _, device := range devices {
		mountPath := device["path"]
		if !strings.HasPrefix(mountPath, "/") {
			mountPath = "/" + mountPath
		}
		sourcePath := device["source"]
		fmt.Printf("%s on: %s\n", sourcePath, mountPath)
	}

	return nil
}

// DeleteUnit ..
func (bh *BraveHost) DeleteUnit(name string) error {
	var unitNames []string

	remoteName, name := ParseRemoteName(name)

	// If local remote, ensure the VM is started
	if remoteName == shared.BravetoolsRemote {
		err := bh.Backend.Start()
		if err != nil {
			return errors.New("Failed to start backend: " + err.Error())
		}
	}

	remote, err := LoadRemoteSettings(remoteName)
	if err != nil {
		return err
	}

	lxdServer, err := GetLXDInstanceServer(remote)
	if err != nil {
		return err
	}

	inst, _, err := lxdServer.GetInstance(name)
	if err != nil {
		return err
	}

	for deviceName, d := range inst.Devices {
		if (d["type"] == "disk") && strings.HasPrefix(deviceName, "brave_") {
			err = bh.UmountShare(name, d["path"])
			if err != nil {
				log.Println(err)
			}
		}
	}

	// Remote profile to filter units by - try using deploy profile. If not, fallback to brave host profile.
	remoteProfile := remote.Profile
	if remoteProfile == "" {
		remoteProfile = bh.Remote.Profile
	}

	unitList, err := GetUnits(lxdServer, remoteProfile)
	if err != nil {
		return errors.New("failed to list existing units: " + err.Error())
	}

	for _, u := range unitList {
		unitNames = append(unitNames, u.Name)
	}

	if !shared.StringInSlice(name, unitNames) {
		return errors.New("unit " + name + " does not exist")
	}

	err = DeleteUnit(lxdServer, name)
	if err != nil {
		return errors.New("failed to delete unit: " + err.Error())
	}

	// Deleting unit from databse

	userHome, err := os.UserHomeDir()
	if err != nil {
		return errors.New("failed to get home directory")
	}
	dbPath := path.Join(userHome, shared.BraveDB)
	database, err := db.OpenDB(dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database %s", dbPath)
	}

	err = db.DeleteUnitDB(database, name)
	if err != nil {
		return errors.New("failed to delete unit from database. Name: " + name + " Error: " + err.Error())
	}

	return nil
}

type ImageExistsError struct {
	Name string
}

func (e *ImageExistsError) Error() string {
	return fmt.Sprintf("image %q already exists", e.Name)
}

// BuildImage creates an image based on Bravefile
func (bh *BraveHost) BuildImage(bravefile shared.Bravefile) error {
	if bh.Remote.Name == shared.BravetoolsRemote {
		err := bh.Backend.Start()
		if err != nil {
			return errors.New("failed to get host info: " + err.Error())
		}
	}

	err := buildImage(bh, &bravefile)

	switch err.(type) {
	case nil:
	case *ImageExistsError:
		if !needTransferImage(bravefile) {
			return err
		}
	default:
		return err
	}

	return TransferImage(bh.Remote, bravefile)
}

// PublishUnit publishes unit to image
func (bh *BraveHost) PublishUnit(unitName string, imageName string) error {
	remoteName, unitName := ParseRemoteName(unitName)
	remote, err := LoadRemoteSettings(remoteName)
	if err != nil {
		return err
	}

	if remote.Name == shared.BravetoolsRemote {
		err := bh.Backend.Start()
		if err != nil {
			return errors.New("failed to get host info: " + err.Error())
		}
	}

	lxdServer, err := GetLXDInstanceServer(remote)
	if err != nil {
		return err
	}

	serverArch, err := GetLXDServerArch(lxdServer)
	if err != nil {
		return errors.New("failed to determine LXD server CPU architecture")
	}

	if imageName == "" {
		timestamp := time.Now()
		imageName = unitName + "/" + timestamp.Format("20060102150405") + "/" + serverArch
	}

	imageStruct, err := ParseImageString(imageName)
	if err != nil {
		return fmt.Errorf("failed to parse image string %q: %s", imageName, err)
	}

	if imageStruct.Version == "" {
		imageStruct.Version = defaultImageVersion
	}
	if imageStruct.Architecture == "" {
		imageStruct.Architecture = serverArch
	}

	imageName = imageStruct.ToBasename()

	// Create an image based on running container and export it. Image saved as tar.gz in project local directory.
	fmt.Printf("Publishing unit %q as image %q\n", unitName, imageName+".tar.gz")

	unitFingerprint, err := Publish(lxdServer, unitName, imageName)
	defer DeleteImageByFingerprint(lxdServer, unitFingerprint)
	if err != nil {
		return errors.New("failed to publish image: " + err.Error())
	}

	fmt.Println("Exporting archive ...")
	err = ExportImage(lxdServer, unitFingerprint, imageName)
	if err != nil {
		return errors.New("failed to export unit: " + err.Error())
	}

	fmt.Println("Cleaning ...")

	return nil
}

func ExportBravetoolsImage(image string, outputDir string) error {
	img, err := ParseImageString(image)
	if err != nil {
		return err
	}

	path, err := matchLocalImagePath(img)
	if err != nil {
		return err
	}

	resolvedImg, err := ImageFromFilename(filepath.Base(path))
	if err != nil {
		resolvedImg, err = ImageFromLegacyFilename(path)
		if err != nil {
			resolvedImg = img
		}
	}

	destPath := filepath.Base(path)
	if outputDir != "" {
		destPath = filepath.Join(outputDir, destPath)
	}

	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("existing file at %s would be overwritten by export of %q", destPath, resolvedImg)
	}

	err = shared.CopyFile(path, destPath)
	if err != nil {
		return err
	}

	fmt.Printf("Exported image %q to: %s\n", resolvedImg, destPath)

	return nil
}

// StopUnit stops unit using name
func (bh *BraveHost) StopUnit(name string) error {

	remoteName, name := ParseRemoteName(name)

	// If local remote, ensure the VM is started
	if remoteName == shared.BravetoolsRemote {
		err := bh.Backend.Start()
		if err != nil {
			return errors.New("Failed to start backend: " + err.Error())
		}
	}

	remote, err := LoadRemoteSettings(remoteName)
	if err != nil {
		return err
	}

	lxdServer, err := GetLXDInstanceServer(remote)
	if err != nil {
		return err
	}

	fmt.Println("Stopping unit: ", name)
	err = Stop(lxdServer, name)
	if err != nil {
		return errors.New("failed to stop unit: " + err.Error())
	}

	return nil
}

// StartUnit restarts unit if running and starts if stopped.
func (bh *BraveHost) StartUnit(name string) error {
	remoteName, name := ParseRemoteName(name)

	// If local remote, ensure the VM is started
	if remoteName == shared.BravetoolsRemote {
		err := bh.Backend.Start()
		if err != nil {
			return errors.New("failed to start backend: " + err.Error())
		}
	}

	remote, err := LoadRemoteSettings(remoteName)
	if err != nil {
		return err
	}

	lxdServer, err := GetLXDInstanceServer(remote)
	if err != nil {
		return err
	}

	fmt.Println("Starting unit: ", name)
	err = Start(lxdServer, name)
	if err != nil {
		return errors.New("failed to start unit: " + err.Error())
	}

	return nil
}

// InitUnit starts unit from supplied image
func (bh *BraveHost) InitUnit(backend Backend, unitParams shared.Service) (err error) {
	// Check for missing mandatory fields
	err = unitParams.ValidateDeploy()
	if err != nil {
		return err
	}

	fmt.Println(shared.Info("Deploying Unit " + unitParams.Name))

	// Intercept SIGINT and cancel context, triggering cleanup of resources
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range c {
			fmt.Println("Interrupting deployment and cleaning artefacts")
			cancel()
		}
	}()

	var imageStruct BravetoolsImage

	// If version explicitly provided separately this is a legacy Bravefile
	if unitParams.Version == "" {
		imageStruct, err = ParseImageString(unitParams.Image)
	} else {
		imageStruct, err = ParseLegacyImageString(unitParams.Image)
	}
	if err != nil {
		return err
	}

	// Parse image location and pull from remote server to local bravetools image store if needed
	var imageRemoteName string
	imageRemoteName, unitParams.Image = ParseRemoteName(unitParams.Image)

	if imageRemoteName == shared.BravetoolsRemote {
		err := bh.Backend.Start()
		if err != nil {
			return errors.New("failed to get host info: " + err.Error())
		}
	}

	if imageRemoteName != shared.BravetoolsRemote {
		bravefile := shared.NewBravefile()
		bravefile.Image = imageStruct.String()
		bravefile.Base.Image = imageRemoteName + ":" + imageStruct.String()
		bravefile.Base.Location = "private"

		bravefile.PlatformService.Name = ""
		bravefile.PlatformService.Image = imageStruct.String()

		err = bh.BuildImage(*bravefile)
		switch errType := err.(type) {
		case nil:
		case *ImageExistsError:
			// If image already exists continue and log the skip
			err = nil
			fmt.Printf("image %q already exists locally - skipping remote import\n", errType.Name)
		default:
			// Stop on unknown err
			return err
		}
	}

	// Connect to deploy target remote
	deployRemoteName, unitName := ParseRemoteName(unitParams.Name)
	unitParams.Name = unitName

	// If local remote, check if running
	if deployRemoteName == shared.BravetoolsRemote {
		err := bh.Backend.Start()
		if err != nil {
			return errors.New("Failed to start backend: " + err.Error())
		}
	}

	deployRemote, err := LoadRemoteSettings(deployRemoteName)

	if err != nil {
		return fmt.Errorf("failed to load remote %q for requested unit %q: %s", deployRemoteName, unitName, err.Error())
	}

	// Load remote defaults for LXD resources for deployment (profile, network, storage) if not specified in Bravefile unitParams
	if unitParams.Profile == "" {
		unitParams.Profile = deployRemote.Profile
	}
	if unitParams.Network == "" {
		unitParams.Network = deployRemote.Network
	}
	if unitParams.Storage == "" {
		unitParams.Storage = deployRemote.Storage
	}

	// As last resort if not provided in Bravefile or remote, try the Brave host settings - mostly for backward compatability
	if unitParams.Profile == "" && unitParams.Network == "" && unitParams.Storage == "" {
		unitParams.Profile = bh.Settings.Profile
		unitParams.Network = bh.Settings.Name
		unitParams.Storage = bh.Settings.StoragePool.Name
	}

	lxdServer, err := GetLXDInstanceServer(deployRemote)
	if err != nil {
		return err
	}

	// Check for existing container with this unit name
	if _, _, err := lxdServer.GetInstance(unitName); err == nil {
		return fmt.Errorf("container with name %q is already running on %q remote", unitName, deployRemoteName)
	}

	deployArch, err := GetLXDServerArch(lxdServer)
	if err != nil {
		return err
	}

	if imageStruct.Architecture == "" {
		imageStruct.Architecture = deployArch
	}

	image, err := matchLocalImagePath(imageStruct)
	if err != nil {
		return err
	}

	imgSize, err := localImageSize(imageStruct)
	if err != nil {
		return fmt.Errorf("failed to get image size for image %q", imageStruct.String())
	}

	fingerprint, err := shared.FileSha256Hash(image)
	if err != nil {
		return fmt.Errorf("failed to obtain image hash %q", unitParams.Image)
	}

	// Resource checks
	if unitParams.Storage != "" {
		err = CheckStoragePoolSpace(lxdServer, unitParams.Storage, imgSize)
		if err != nil {
			return err
		}
	}
	err = CheckMemory(lxdServer, unitParams.Resources.RAM)
	if err != nil {
		log.Fatal(err.Error())
	}

	if !strings.Contains(deployRemote.URL, "unix.socket") {
		err = CheckHostPorts(deployRemote.URL, unitParams.Ports)
		if err != nil {
			return err
		}
	}

	// Import local image if it doesn't exist in LXD image store
	if _, _, err = lxdServer.GetImage(fingerprint); err != nil {
		_, err = ImportImage(lxdServer, image, unitName)
		unitParams.Image = unitName
		if err = shared.CollectErrors(err, ctx.Err()); err != nil {
			return errors.New("failed to import image: " + err.Error())
		}
		defer DeleteImageByFingerprint(lxdServer, fingerprint)
	}

	// Launch unit and set up cleanup code to delete it if an error encountered during deployment
	_, err = LaunchFromImage(lxdServer, lxdServer, unitParams.Image, unitParams.Name, unitParams.Profile, unitParams.Storage)
	defer func() {
		if err != nil {
			delErr := DeleteUnit(lxdServer, unitName)
			if delErr != nil {
				log.Println("failed to delete unit: " + delErr.Error())
			}
		}
	}()
	if err = shared.CollectErrors(err, ctx.Err()); err != nil {
		return errors.New("failed to launch unit: " + err.Error())
	}

	err = AttachNetwork(lxdServer, unitName, unitParams.Network, "eth0", "eth0")
	if err = shared.CollectErrors(err, ctx.Err()); err != nil {
		return errors.New("failed to attach network: " + err.Error())
	}

	// Assign static IP
	if unitParams.IP != "" {
		err = ConfigDevice(lxdServer, unitName, "eth0", unitParams.IP)
		if err = shared.CollectErrors(err, ctx.Err()); err != nil {
			errMsg := fmt.Sprintf("failed to set IP: %s.\n", err.Error())
			bridgeIP, _ := GetBravetoolsBridgeIP(lxdServer, bh.Settings.Network.Name)
			if bridgeIP != "" {
				errMsg = fmt.Sprintf("%s\nBravetools bridge is available at %s\n", errMsg, bridgeIP)
			}
			return errors.New(errMsg)
		}
	}

	err = Stop(lxdServer, unitName)
	if err = shared.CollectErrors(err, ctx.Err()); err != nil {
		return errors.New("failed to stop unit: " + err.Error())
	}

	err = Start(lxdServer, unitName)
	if err = shared.CollectErrors(err, ctx.Err()); err != nil {
		return errors.New("Failed to restart unit: " + err.Error())
	}

	user, err := user.Current()
	if err != nil {
		return err
	}

	var uid string
	var gid string

	hostOs := runtime.GOOS
	if hostOs == "windows" {
		uidParts := strings.Split(user.Uid, "-")
		gidParts := strings.Split(user.Gid, "-")

		uid = uidParts[len(uidParts)-1]
		gid = gidParts[len(gidParts)-1]
	} else {
		uid = user.Uid
		gid = user.Gid
	}

	serverVersion, err := GetLXDServerVersion(lxdServer)
	if err != nil {
		return errors.New("failed to get server version: " + err.Error())
	}

	// uid and gid mapping is not allowed in non-snap LXD. Shares can be created, but they are read-only in a unit.
	var config map[string]string
	if serverVersion <= 303 {
		config = map[string]string{
			"limits.cpu":       unitParams.Resources.CPU,
			"limits.memory":    unitParams.Resources.RAM,
			"security.nesting": "false",
			"nvidia.runtime":   "false",
		}
	} else {
		config = map[string]string{
			"limits.cpu":       unitParams.Resources.CPU,
			"limits.memory":    unitParams.Resources.RAM,
			"raw.idmap":        "both " + uid + " " + gid,
			"security.nesting": "false",
			"nvidia.runtime":   "false",
		}
	}

	if unitParams.Docker == "yes" {
		config["security.nesting"] = "true"
	}

	if unitParams.Resources.GPU == "yes" {
		config["nvidia.runtime"] = "true"
		device := map[string]string{"type": "gpu"}
		err = AddDevice(lxdServer, unitName, "gpu", device)
		if err != nil {
			return errors.New("failed to add GPU device: " + err.Error())
		}
	}

	err = SetConfig(lxdServer, unitName, config)
	if err = shared.CollectErrors(err, ctx.Err()); err != nil {
		return errors.New("error configuring unit: " + err.Error())
	}

	err = Stop(lxdServer, unitName)
	if err = shared.CollectErrors(err, ctx.Err()); err != nil {
		return errors.New("failed to stop unit: " + err.Error())
	}

	err = Start(lxdServer, unitName)
	if err = shared.CollectErrors(err, ctx.Err()); err != nil {
		return errors.New("failed to restart unit: " + err.Error())
	}

	ports := unitParams.Ports
	if len(ports) > 0 {
		for _, p := range ports {
			ps := strings.Split(p, ":")
			if len(ps) < 2 {
				err = errors.New("invalid port forwarding definition. Appropriate format is UNIT_PORT:HOST_PORT")
				return
			}

			err = addIPRules(lxdServer, unitName, ps[1], ps[0])
			if err = shared.CollectErrors(err, ctx.Err()); err != nil {
				return errors.New("unable to add Proxy Device: " + err.Error())
			}
		}
	}

	err = postdeploy(ctx, lxdServer, &unitParams)
	if err = shared.CollectErrors(err, ctx.Err()); err != nil {
		return err
	}

	// Add unit into database

	var braveUnit db.BraveUnit
	userHome, err := os.UserHomeDir()
	if err != nil {
		return errors.New("failed to get home directory")
	}
	dbPath := path.Join(userHome, shared.BraveDB)

	database, err := db.OpenDB(dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database %s", dbPath)
	}

	uuid, _ := uuid.NewUUID()
	braveUnit.UID = uuid.String()
	braveUnit.Name = unitName
	braveUnit.Date = time.Now().String()

	var unitData db.UnitData
	unitData.CPU, _ = strconv.Atoi(unitParams.Resources.CPU)
	unitData.RAM = unitParams.Resources.RAM
	unitData.IP = unitParams.IP
	unitData.Image = unitParams.Image

	data, err := json.Marshal(unitData)
	if err != nil {
		return errors.New("failed to serialize unit data")
	}
	braveUnit.Data = data

	_, err = db.InsertUnitDB(database, braveUnit)
	if err != nil {
		return errors.New("failed to insert unit to database: " + err.Error())
	}

	return nil
}

func (bh *BraveHost) Compose(backend Backend, composeFile *shared.ComposeFile) (err error) {

	// Compose runs from parent directory of compose file
	workingDir, err := filepath.Abs(filepath.Dir(composeFile.Path))
	if err != nil {
		return err
	}
	startDir, err := os.Getwd()
	if err != nil {
		return err
	}
	os.Chdir(workingDir)
	defer os.Chdir(startDir)

	// Order services by deps
	topologicalOrdering, err := composeFile.TopologicalOrdering()
	if err != nil {
		return err
	}

	// Remove base-only services if all images depending on them already exist
	for _, baseService := range getBaseOnlyServices(composeFile) {
		dependentServices, err := getBuildDependents(baseService, composeFile)
		if err != nil {
			return err
		}
		if len(dependentServices) == 0 {
			serviceIdx, err := shared.StrSliceIndexOf(topologicalOrdering, baseService)
			if err != nil {
				return err
			}
			topologicalOrdering = append(topologicalOrdering[:serviceIdx], topologicalOrdering[serviceIdx+1:]...)
		}
	}

	// Validate Services
	for _, serviceName := range topologicalOrdering {
		service, exist := composeFile.Services[serviceName]
		if !exist {
			err = fmt.Errorf("service name %q does not exist in Services", serviceName)
			return err
		}
		err = service.ValidateDeploy()
		if err != nil {
			err = fmt.Errorf("failed to deploy service %q: %s", serviceName, err)
			return err
		}
	}

	// (Optionally build) and deploy each service
	for _, serviceName := range topologicalOrdering {
		service := composeFile.Services[serviceName]

		// Load bravefile settings as defaults, overwrite if specified in composefile
		if service.Bravefile != "" {
			if service.Build || service.Base {
				err = service.BravefileBuild.ValidateBuild()
				if err != nil {
					return fmt.Errorf("invalid Bravefile for service %q: %s", service.Name, err)
				}

				// Switch to build context dir
				buildDir := service.Context
				if buildDir == "" {
					buildDir, err = filepath.Abs(filepath.Dir(service.Bravefile))
				}
				os.Chdir(buildDir)

				err = bh.BuildImage(*service.BravefileBuild)
				switch errType := err.(type) {
				case nil:
					// Cleanup image later if error in compose
					defer func() {
						if err != nil {
							bh.DeleteLocalImage(service.Image, service.BravefileBuild.IsLegacy())
						}
					}()
				case *ImageExistsError:
					// If image already exists continue and log the skip
					err = nil
					fmt.Printf("image %q already exists - skipping build\n", errType.Name)
				default:
					// Stop on unknown err
					return err
				}

				os.Chdir(workingDir)

				if service.Base && !service.Build {
					defer func() {
						bh.DeleteLocalImage(service.Image, service.BravefileBuild.IsLegacy())
					}()
				}
			}
		}

		// Only deploy service if it isn't a base image used during build only
		if !service.Base {
			// Deploy context - use Context if provided, else Bravefile if present, else current dir
			deployDir := service.Context
			if deployDir == "" {
				if service.Bravefile != "" {
					deployDir, err = filepath.Abs(filepath.Dir(service.Bravefile))
					if err != nil {
						return err
					}
				} else {
					deployDir = "."
				}
			}
			os.Chdir(deployDir)

			// Cleanup each unit if error in compose
			err = bh.InitUnit(backend, service.Service)
			if err != nil {
				return err
			}
			defer func() {
				if err != nil {
					bh.DeleteUnit(service.Name)
				}
			}()

			os.Chdir(workingDir)
		}

	}
	return nil
}
