package protocol

type InstanceSpec struct {
	CPU      int    `json:"cpu"`
	MemoryMB int    `json:"memory_mb"`
	DiskGB   int    `json:"disk_gb"`
	Arch     string `json:"arch,omitempty"`
}

type Image struct {
	ID           uint   `json:"id,omitempty"`
	Name         string `json:"name"`
	DisplayName  string `json:"display_name,omitempty"`
	Driver       string `json:"driver"`
	Type         string `json:"type"`
	OriginalName string `json:"original_name,omitempty"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
	Checksum     string `json:"checksum,omitempty"`
	Path         string `json:"path,omitempty"`
}

type Instance struct {
	ID          uint         `json:"id"`
	Name        string       `json:"name"`
	DisplayName string       `json:"display_name,omitempty"`
	Driver      string       `json:"driver"`
	Type        string       `json:"type"`
	Status      string       `json:"status,omitempty"`
	ImageID     *uint        `json:"image_id,omitempty"`
	Spec        InstanceSpec `json:"spec"`
	Image       *Image       `json:"image,omitempty"`
}
