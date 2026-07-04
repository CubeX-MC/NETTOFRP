package selector

import "math"

// regionCoords 是区域标记到其地理中心（纬度, 经度）的映射，用于「就近」兜底排序。
// 键为 ISO 3166-2 省级代码（如 "CN-ZJ"）或国家码（如 "CN"）。坐标取该行政区
// 首府/几何中心的近似值，仅用于相对距离比较，无需高精度。
var regionCoords = map[string][2]float64{
	// 中国大陆各省级行政区（省会/首府近似坐标）
	"CN-BJ": {39.90, 116.41}, // 北京
	"CN-TJ": {39.13, 117.20}, // 天津
	"CN-HE": {38.04, 114.51}, // 河北 石家庄
	"CN-SX": {37.87, 112.55}, // 山西 太原
	"CN-NM": {40.84, 111.75}, // 内蒙古 呼和浩特
	"CN-LN": {41.80, 123.43}, // 辽宁 沈阳
	"CN-JL": {43.90, 125.32}, // 吉林 长春
	"CN-HL": {45.80, 126.53}, // 黑龙江 哈尔滨
	"CN-SH": {31.23, 121.47}, // 上海
	"CN-JS": {32.06, 118.80}, // 江苏 南京
	"CN-ZJ": {30.27, 120.15}, // 浙江 杭州
	"CN-AH": {31.86, 117.28}, // 安徽 合肥
	"CN-FJ": {26.07, 119.30}, // 福建 福州
	"CN-JX": {28.68, 115.86}, // 江西 南昌
	"CN-SD": {36.67, 117.00}, // 山东 济南
	"CN-HA": {34.76, 113.65}, // 河南 郑州
	"CN-HB": {30.59, 114.30}, // 湖北 武汉
	"CN-HN": {28.23, 112.94}, // 湖南 长沙
	"CN-GD": {23.13, 113.26}, // 广东 广州
	"CN-GX": {22.82, 108.32}, // 广西 南宁
	"CN-HI": {20.04, 110.20}, // 海南 海口
	"CN-CQ": {29.56, 106.55}, // 重庆
	"CN-SC": {30.57, 104.07}, // 四川 成都
	"CN-GZ": {26.65, 106.63}, // 贵州 贵阳
	"CN-YN": {25.04, 102.71}, // 云南 昆明
	"CN-XZ": {29.65, 91.14},  // 西藏 拉萨
	"CN-SN": {34.34, 108.94}, // 陕西 西安
	"CN-GS": {36.06, 103.83}, // 甘肃 兰州
	"CN-QH": {36.62, 101.78}, // 青海 西宁
	"CN-NX": {38.47, 106.28}, // 宁夏 银川
	"CN-XJ": {43.83, 87.62},  // 新疆 乌鲁木齐
	"CN-HK": {22.32, 114.17}, // 香港
	"CN-MO": {22.20, 113.54}, // 澳门
	"CN-TW": {25.03, 121.57}, // 台湾 台北
	// 国家级中心（无省份信息时的近似）
	"CN": {35.86, 104.20}, // 中国几何中心近似
}

// regionCoord 返回区域标记的地理坐标。优先精确匹配（省级），未命中时回退国家级。
func regionCoord(region string) (lat, lon float64, ok bool) {
	if c, has := regionCoords[region]; has {
		return c[0], c[1], true
	}
	// 省级未收录时，尝试用国家码兜底（如 "CN-XX" -> "CN"）。
	if i := indexDash(region); i > 0 {
		if c, has := regionCoords[region[:i]]; has {
			return c[0], c[1], true
		}
	}
	return 0, 0, false
}

func indexDash(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '-' {
			return i
		}
	}
	return -1
}

// haversine 返回地球表面两点间的大圆距离（公里），用于相对就近比较。
func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0 // 地球半径（公里）
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}
