#!/usr/bin/env gnuplot

set term postscript eps enhanced color font 'Helvetica,22' linewidth 2
set output '../figure/fcnt.eps'
set style data points
set key inside right top vertical
set key font ",20" width 3
set xtics 64
set ytics 1500
set ylabel "Access Frequency"
set xlabel "PTE Index"
set style fill pattern
plot '../data/fcnt.dat' using 2 with impulses title "PTE Access" lt 7
