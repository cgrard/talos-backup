// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package config provides functions for parsing the
// backup configs
package config

// S3Info is the struct to hold info on where to push in s3.
type S3Info struct {
	Bucket string `yaml:"bucket"`
}
