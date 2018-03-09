#!/usr/bin/env gnuplot

set term postscript eps enhanced color font 'Helvetica,20' linewidth 2
set output '../figure/fts.eps'
# set style histogram cluster gap 1
#set key inside right top vertical
set nokey
set yrange [0:512]
set xrange [0:10]
set format x "%.0f" 
#set xtics rotate by 45 out offset 0,-2.0
set ytics 128

# set ytics mirror
set ylabel "PTE index"
set xlabel "Time"
plot '../data/new2.dat' lt 7 pt 7
