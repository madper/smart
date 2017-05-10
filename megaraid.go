/*
 * Pure Go SMART library
 * Copyright 2017 Daniel Swarbrick
 *
 * Broadcom (formerly Avago, LSI) MegaRAID ioctl functions
 * TODO:
 * - Improve code comments, refer to in-kernel structs
 * - Device Scan:
 *   - Walk /sys/class/scsi_host/ directory
 *   - "host%d" symlinks enumerate hosts
 *   - "host%d/proc_name" should contain the value "megaraid_sas"
 */

package smart

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const (
	MAX_IOCTL_SGE = 16

	MFI_CMD_DCMD = 0x05

	MR_DCMD_PD_GET_LIST = 0x02010000 // Obsolete / deprecated command
)

type megasas_sge64 struct {
	phys_addr uint32
	length    uint32
	_padding  uint32
}

type Iovec struct {
	Base uint64 // FIXME: This is not portable to 32-bit platforms!
	Len  uint64
}

type megasas_dcmd_frame struct {
	cmd           uint8
	reserved_0    uint8
	cmd_status    uint8
	reserved_1    [4]uint8
	sge_count     uint8
	context       uint32
	pad_0         uint32
	flags         uint16
	timeout       uint16
	data_xfer_len uint32
	opcode        uint32
	mbox          [12]byte      // FIXME: This is actually a union of [12]uint8 / [6]uint16 / [3]uint32
	sgl           megasas_sge64 // FIXME: This is actually a union of megasas_sge64 / megasas_sge32
}

type megasas_iocpacket struct {
	host_no   uint16
	__pad1    uint16
	sgl_off   uint32
	sge_count uint32
	sense_off uint32
	sense_len uint32
	// FIXME: This is actually a union of megasas_header / megasas_pthru_frame / megasas_dcmd_frame
	frame [128]byte
	// FIXME: Go is inserting 4 bytes of padding before this in order to 64-bit align the sgl member
	sgl [MAX_IOCTL_SGE]Iovec
}

// Megasas physical device address
type MegasasPDAddress struct {
	DeviceId          uint16
	EnclosureId       uint16
	EnclosureIndex    uint8
	SlotNumber        uint8
	SCSIDevType       uint8
	ConnectPortBitmap uint8
	SASAddr           [2]uint64
}

// Holder for megasas ioctl device
type MegasasIoctl struct {
	DeviceMajor int
	fd          int
}

var (
	// 0xc1944d01 - Beware: cannot use unsafe.Sizeof(megasas_iocpacket{}) due to Go struct padding!
	MEGASAS_IOC_FIRMWARE = _iowr('M', 1, 404)
)

// MakeDev returns the device ID for the specified major and minor numbers, equivalent to
// makedev(3). Based on gnu_dev_makedev macro, may be platform dependent!
func MakeDev(major, minor uint) uint {
	return (minor & 0xff) | ((major & 0xfff) << 8) |
		((minor &^ 0xff) << 12) | ((major &^ 0xfff) << 32)
}

// PackedBytes is a convenience method that will pack a megasas_iocpacket struct in little-endian
// format and return it as a byte slice
func (ioc *megasas_iocpacket) PackedBytes() []byte {
	b := new(bytes.Buffer)
	binary.Write(b, binary.LittleEndian, ioc)
	return b.Bytes()
}

// CreateMegasasIoctl determines the device ID for the MegaRAID SAS ioctl device, creates it
// if necessary, and returns a MegasasIoctl struct to manage the device.
func CreateMegasasIoctl() (MegasasIoctl, error) {
	var (
		m   MegasasIoctl
		err error
	)

	// megaraid_sas driver does not automatically create ioctl device node, so find out the device
	// major number and create it.
	if file, err := os.Open("/proc/devices"); err == nil {
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			if strings.HasSuffix(scanner.Text(), "megaraid_sas_ioctl") {
				if _, err := fmt.Sscanf(scanner.Text(), "%d", &m.DeviceMajor); err == nil {
					break
				}
			}
		}

		if m.DeviceMajor == 0 {
			log.Println("Could not determine megaraid major number!")
			return m, nil
		}

		syscall.Mknod("/dev/megaraid_sas_ioctl_node", syscall.S_IFCHR, int(MakeDev(uint(m.DeviceMajor), 0)))
	} else {
		return m, err
	}

	m.fd, err = syscall.Open("/dev/megaraid_sas_ioctl_node", syscall.O_RDWR, 0600)

	if err != nil {
		return m, err
	}

	return m, nil
}

// Close closes the file descriptor of the MegasasIoctl instance
func (m *MegasasIoctl) Close() {
	syscall.Close(m.fd)
}

// MFI sends a MegaRAID Firmware Interface (MFI) command to the specified host
func (m *MegasasIoctl) MFI(host uint16, opcode uint32, b []byte) error {
	var ioc megasas_iocpacket

	ioc.host_no = host

	// Approximation of C union behaviour
	dcmd := (*megasas_dcmd_frame)(unsafe.Pointer(&ioc.frame))
	dcmd.cmd = MFI_CMD_DCMD
	dcmd.opcode = opcode
	dcmd.data_xfer_len = uint32(len(b))
	dcmd.sge_count = 1

	ioc.sge_count = 1
	ioc.sgl_off = uint32(unsafe.Offsetof(dcmd.sgl))
	ioc.sgl[0] = Iovec{uint64(uintptr(unsafe.Pointer(&b[0]))), uint64(len(b))}

	iocBuf := ioc.PackedBytes()

	// Note pointer to first item in iocBuf buffer
	if err := ioctl(uintptr(m.fd), MEGASAS_IOC_FIRMWARE, uintptr(unsafe.Pointer(&iocBuf[0]))); err != nil {
		return err
	}

	return nil
}

// GetDeviceList retrieves a list of physical devices attached to the specified host
func (m *MegasasIoctl) GetDeviceList(host uint16) ([]MegasasPDAddress, error) {
	respBuf := make([]byte, 4096)

	if err := m.MFI(0, MR_DCMD_PD_GET_LIST, respBuf); err != nil {
		log.Println(err)
		return nil, err
	}

	respCount := nativeEndian.Uint32(respBuf[4:])

	// Create a device array large enough to hold the specified number of devices
	devices := make([]MegasasPDAddress, respCount)
	binary.Read(bytes.NewBuffer(respBuf[8:]), nativeEndian, &devices)

	return devices, nil
}

func OpenMegasasIoctl() error {
	m, _ := CreateMegasasIoctl()
	fmt.Printf("%#v\n", m)

	defer m.Close()

	// FIXME: Don't assume that host is always zero
	devices, _ := m.GetDeviceList(0)

	fmt.Println("\nEncl.  Slot  Device Id  SAS Address")
	for _, pd := range devices {
		if pd.SCSIDevType == 0 { // SCSI disk
			fmt.Printf("%5d   %3d      %5d  %#x\n", pd.EnclosureId, pd.SlotNumber, pd.DeviceId, pd.SASAddr[0])
		}
	}

	return nil
}