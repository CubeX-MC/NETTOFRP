// Package geoip 基于 MaxMind GeoLite2-City 数据库，将玩家 IP 解析为地理坐标及区域标记，
// 供选路时按玩家真实位置就近选线。区域标记形如 "CN-ZJ"（国家-省份）或 "CN"（仅国家）。
package geoip

import (
	"fmt"
	"net"

	"github.com/oschwald/maxminddb-golang"
)

// DB 封装一个只读的 GeoLite2-City 数据库。
type DB struct {
	reader *maxminddb.Reader
}

// Open 加载指定路径的 .mmdb 文件。
func Open(path string) (*DB, error) {
	r, err := maxminddb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开 GeoIP 数据库失败: %w", err)
	}
	return &DB{reader: r}, nil
}

// Close 释放数据库资源。
func (d *DB) Close() error {
	if d == nil || d.reader == nil {
		return nil
	}
	return d.reader.Close()
}

// cityRecord 取选路所需字段：国家/一级行政区 ISO 码，以及玩家经纬度。
// 经纬度是就近选路的主依据，即便省份 ISO 缺失通常也能取到，比省份码可靠得多。
type cityRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	Subdivisions []struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"subdivisions"`
	Location struct {
		Latitude  float64 `maxminddb:"latitude"`
		Longitude float64 `maxminddb:"longitude"`
	} `maxminddb:"location"`
}

// Location 是一次 IP 定位的结果。Region 仅供日志展示，就近选路以 Lat/Lon 为准。
// HasCoord 为 false 时表示未取到经纬度，调用方应退回按区域标记选路。
type Location struct {
	Region   string  // 区域标记，如 "CN-ZJ" 或 "CN"，无法识别时为空
	Lat      float64 // 玩家纬度
	Lon      float64 // 玩家经度
	HasCoord bool    // 是否成功取到经纬度
}

// Locate 解析该 IP 的地理位置。优先返回经纬度（就近选路主依据），
// 同时给出区域标记供日志展示。无法解析或数据库未加载时返回零值 Location。
func (d *DB) Locate(ip net.IP) Location {
	if d == nil || d.reader == nil || ip == nil {
		return Location{}
	}
	var rec cityRecord
	if err := d.reader.Lookup(ip, &rec); err != nil {
		return Location{}
	}

	var loc Location
	// 经纬度：MaxMind 缺省时两者均为 0（(0,0) 落在几内亚湾无人海域），
	// 以此判定为"无坐标"，避免把定位失败的玩家当成站在赤道。
	if rec.Location.Latitude != 0 || rec.Location.Longitude != 0 {
		loc.Lat = rec.Location.Latitude
		loc.Lon = rec.Location.Longitude
		loc.HasCoord = true
	}

	if country := rec.Country.ISOCode; country != "" {
		if len(rec.Subdivisions) > 0 && rec.Subdivisions[0].ISOCode != "" {
			loc.Region = country + "-" + rec.Subdivisions[0].ISOCode
		} else {
			loc.Region = country
		}
	}
	return loc
}
