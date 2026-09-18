package timezones

import "strings"

// Names is a compact inventory of commonly deployed IANA timezone IDs.
// time.LoadLocation remains the authority for behavior; this list gives
// agent workflows a deterministic cross-platform "tz list" surface.
var Names = []string{
	"Africa/Abidjan", "Africa/Accra", "Africa/Algiers", "Africa/Cairo", "Africa/Casablanca", "Africa/Johannesburg", "Africa/Lagos", "Africa/Nairobi",
	"America/Anchorage", "America/Argentina/Buenos_Aires", "America/Bogota", "America/Caracas", "America/Chicago", "America/Denver", "America/Detroit", "America/Halifax", "America/Indiana/Indianapolis", "America/Jamaica", "America/Juneau", "America/La_Paz", "America/Lima", "America/Los_Angeles", "America/Mexico_City", "America/Montevideo", "America/New_York", "America/Phoenix", "America/Puerto_Rico", "America/Santiago", "America/Sao_Paulo", "America/St_Johns", "America/Toronto", "America/Vancouver",
	"Antarctica/McMurdo",
	"Asia/Almaty", "Asia/Amman", "Asia/Baghdad", "Asia/Baku", "Asia/Bangkok", "Asia/Beirut", "Asia/Colombo", "Asia/Dhaka", "Asia/Dubai", "Asia/Ho_Chi_Minh", "Asia/Hong_Kong", "Asia/Jakarta", "Asia/Jerusalem", "Asia/Karachi", "Asia/Kathmandu", "Asia/Kolkata", "Asia/Kuala_Lumpur", "Asia/Manila", "Asia/Riyadh", "Asia/Seoul", "Asia/Shanghai", "Asia/Singapore", "Asia/Taipei", "Asia/Tashkent", "Asia/Tehran", "Asia/Tokyo", "Asia/Yangon",
	"Atlantic/Azores", "Atlantic/Reykjavik",
	"Australia/Adelaide", "Australia/Brisbane", "Australia/Darwin", "Australia/Hobart", "Australia/Melbourne", "Australia/Perth", "Australia/Sydney",
	"Etc/GMT", "Etc/UTC",
	"Europe/Amsterdam", "Europe/Athens", "Europe/Berlin", "Europe/Brussels", "Europe/Bucharest", "Europe/Budapest", "Europe/Copenhagen", "Europe/Dublin", "Europe/Helsinki", "Europe/Istanbul", "Europe/Lisbon", "Europe/London", "Europe/Madrid", "Europe/Moscow", "Europe/Oslo", "Europe/Paris", "Europe/Prague", "Europe/Rome", "Europe/Stockholm", "Europe/Vienna", "Europe/Warsaw", "Europe/Zurich",
	"Indian/Maldives", "Pacific/Auckland", "Pacific/Fiji", "Pacific/Guam", "Pacific/Honolulu", "Pacific/Pago_Pago", "Pacific/Tahiti",
	"UTC",
}

func Filter(substr string) []string {
	if substr == "" {
		return append([]string(nil), Names...)
	}
	needle := strings.ToLower(substr)
	var out []string
	for _, name := range Names {
		if strings.Contains(strings.ToLower(name), needle) {
			out = append(out, name)
		}
	}
	return out
}
