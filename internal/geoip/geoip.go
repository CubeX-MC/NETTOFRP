// Package geoip 基于 MaxMind GeoLite2-City 数据库，将玩家 IP 解析为区域标记，
// 供选路时优先匹配同区线路。区域标记形如 "CN-ZJ"（国家-省份）或 "CN"（仅国家）。
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

// cityRecord 仅取选路所需的最小字段：国家 ISO 码与一级行政区 ISO 码。
type cityRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	Subdivisions []struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"subdivisions"`
}

// Region 返回该 IP 的区域标记：优先 "国家-省份"（如 "CN-ZJ"），
// 无省份信息时退回国家码（如 "CN"）。无法解析时返回空串。
func (d *DB) Region(ip net.IP) string {
	if d == nil || d.reader == nil || ip == nil {
		return ""
	}
	var rec cityRecord
	if err := d.reader.Lookup(ip, &rec); err != nil {
		return ""
	}
	country := rec.Country.ISOCode
	if country == "" {
		return ""
	}
	if len(rec.Subdivisions) > 0 && rec.Subdivisions[0].ISOCode != "" {
		return country + "-" + rec.Subdivisions[0].ISOCode
	}
	return country
}
