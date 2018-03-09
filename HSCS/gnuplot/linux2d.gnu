#!/usr/bin/env gnuplot

set term postscript eps enhanced color font 'Helvetica,20' linewidth 2
set output '../figure/linux2d.eps'
set style data histograms

set xtics border in scale 0,0 nomirror rotate by -90 autojustify
set style fill solid 1.00 border lt -1
set key outside horizontal center top

#set xlabel "Platform"
set ylabel "% of Native Performance"
set yrange [0:100]
set format y '%2.0f%%'

plot '../data/linux2d.dat' using (100*$2/$3):xtic(1) ti col, \
	'' u (100*$2/$4) ti col
